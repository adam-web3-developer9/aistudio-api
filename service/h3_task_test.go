package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// External DSNs must name disposable test databases. The regular package
// fixture provides SQLite; the same accounting cases then run on each real DB.
func setupH3Database(t *testing.T) {
	t.Helper()
	var dialector gorm.Dialector
	dbType := common.DatabaseTypeSQLite
	if dsn := os.Getenv("H3_TEST_MYSQL_DSN"); dsn != "" {
		dialector, dbType = mysql.Open(dsn), common.DatabaseTypeMySQL
	}
	if dsn := os.Getenv("H3_TEST_POSTGRES_DSN"); dsn != "" {
		require.Nil(t, dialector, "set only one H3 database DSN per test run")
		dialector, dbType = postgres.Open(dsn), common.DatabaseTypePostgreSQL
	}
	if dialector == nil {
		return
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dbType, dbType)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	// These are the persisted pre-H3 entities; H3 adds only its ledger table.
	require.NoError(t, db.AutoMigrate(&model.Task{}, &model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
}

type h3ReconciliationAdaptor struct {
	fetch  func(string, string, *model.Task, string) (*http.Response, error)
	result *relaycommon.TaskInfo
}

func (*h3ReconciliationAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *h3ReconciliationAdaptor) FetchTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	return a.fetch(baseURL, key, task, proxy)
}
func (a *h3ReconciliationAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	return a.result, nil
}
func (*h3ReconciliationAdaptor) SanitizeTaskData(body []byte) ([]byte, error) {
	return body, nil
}
func (*h3ReconciliationAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

func setupH3Reconciliation(t *testing.T, adaptor *h3ReconciliationAdaptor) *model.Task {
	t.Helper()
	setupH3Database(t)
	truncate(t)
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(platform constant.TaskPlatform) TaskPollingAdaptor {
		require.EqualValues(t, constant.TaskPlatformH3Studio, platform)
		return adaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })
	seedUser(t, 880, 1_000_000)
	seedToken(t, 880, 880, "h3-user-token", 800_000)
	// Explicit expiry is part of authorization; zero means already expired.
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 880).Update("expired_time", -1).Error)
	seedChannel(t, 880)
	// Upgrade populated pre-H3 tables twice: migration must be repeatable and
	// must not rewrite existing balances or token ownership.
	for range 2 {
		require.NoError(t, model.DB.AutoMigrate(&model.H3TaskReservation{}))
	}
	t.Cleanup(func() { model.DB.Where("1 = 1").Delete(&model.H3TaskReservation{}) })
	assert.Equal(t, 1_000_000, getUserQuota(t, 880))
	assert.Equal(t, 800_000, getTokenRemainQuota(t, 880))
	task := &model.Task{
		TaskID: "task_h3_reconcile", UserId: 880, ChannelId: 880,
		Platform: constant.TaskPlatformH3Studio, Status: model.TaskStatusSubmitted,
		SubmitTime: time.Now().Unix(), Progress: "0%", Quota: 225_000, Group: "default",
		Properties: model.Properties{OriginModelName: "h3"},
		PrivateData: model.TaskPrivateData{
			BillingSource: "wallet", TokenId: 880, Key: "frozen-operator-key",
			H3: &model.H3TaskContext{
				RequestBody: []byte(`{"requestKey":"task_h3_reconcile","values":{"duration":5}}`),
				Seconds:     5, BaseURL: "https://original-studio.example", Proxy: "",
			},
			BillingContext: &model.TaskBillingContext{ModelPrice: 0.09, GroupRatio: 1, OtherRatios: map[string]float64{"seconds": 5}, OriginModelName: "h3", PerCallBilling: true},
		},
	}
	stored, created, err := model.ReserveH3Task(task, "immutable-request-hash")
	require.NoError(t, err)
	require.True(t, created)
	return stored
}

func h3Response(t *testing.T, seconds int) *http.Response {
	t.Helper()
	body, err := common.Marshal(map[string]any{"id": "native-job", "status": "running", "billing": map[string]any{"unit": "second", "requestedSeconds": seconds}})
	require.NoError(t, err)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}
}

func reloadH3Task(t *testing.T, task *model.Task) *model.Task {
	t.Helper()
	stored, exists, err := model.GetByTaskId(task.UserId, task.TaskID)
	require.NoError(t, err)
	require.True(t, exists)
	return stored
}

func TestH3ReconciliationTransportFailureKeepsChargeAndResumesSavedRequest(t *testing.T) {
	adaptor := &h3ReconciliationAdaptor{fetch: func(string, string, *model.Task, string) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	}}
	task := setupH3Reconciliation(t, adaptor)
	require.ErrorIs(t, SyncH3Task(context.Background(), task), context.DeadlineExceeded)
	stored := reloadH3Task(t, task)
	assert.EqualValues(t, model.TaskStatusSubmitted, stored.Status)
	assert.Equal(t, 775_000, getUserQuota(t, 880))
	assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))

	adaptor.result = &relaycommon.TaskInfo{Status: model.TaskStatusQueued, Progress: "10%"}
	adaptor.fetch = func(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
		assert.Equal(t, "https://original-studio.example", baseURL)
		assert.Equal(t, "frozen-operator-key", key)
		assert.Equal(t, stored.TaskID, task.TaskID)
		assert.Equal(t, stored.PrivateData.H3.RequestBody, task.PrivateData.H3.RequestBody)
		return h3Response(t, 5), nil
	}
	require.NoError(t, SyncH3Task(context.Background(), stored))
	assert.EqualValues(t, model.TaskStatusQueued, reloadH3Task(t, stored).Status)
	assert.Equal(t, 775_000, getUserQuota(t, 880))
}

func TestH3ReconciliationMismatchedBillingCannotCompleteOrRefund(t *testing.T) {
	for _, status := range []string{model.TaskStatusSuccess, model.TaskStatusFailure} {
		t.Run(status, func(t *testing.T) {
			adaptor := &h3ReconciliationAdaptor{
				fetch:  func(string, string, *model.Task, string) (*http.Response, error) { return h3Response(t, 10), nil },
				result: &relaycommon.TaskInfo{Status: status, Url: "https://storage.example/video.mp4", Reason: "failed"},
			}
			task := setupH3Reconciliation(t, adaptor)
			require.Error(t, SyncH3Task(context.Background(), task))
			assert.EqualValues(t, model.TaskStatusSubmitted, reloadH3Task(t, task).Status)
			assert.Equal(t, 775_000, getUserQuota(t, 880))
			assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))
		})
	}
}

func TestH3ReconciliationConfirmedFailureRefundsOnce(t *testing.T) {
	adaptor := &h3ReconciliationAdaptor{
		fetch:  func(string, string, *model.Task, string) (*http.Response, error) { return h3Response(t, 5), nil },
		result: &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "PROVIDER_CONFIRMED_FAILURE"},
	}
	task := setupH3Reconciliation(t, adaptor)
	require.NoError(t, SyncH3Task(context.Background(), task))
	assert.EqualValues(t, model.TaskStatusFailure, task.Status)
	assert.Equal(t, "PROVIDER_CONFIRMED_FAILURE", task.FailReason)
	assert.Equal(t, 1_000_000, getUserQuota(t, 880))
	assert.Equal(t, 800_000, getTokenRemainQuota(t, 880))
	assert.Equal(t, int64(1), countLogs(t))
	require.NoError(t, SyncH3Task(context.Background(), reloadH3Task(t, task)))
	assert.Equal(t, 1_000_000, getUserQuota(t, 880))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestH3TimeoutSweepKeepsPendingVideoAndReservation(t *testing.T) {
	task := setupH3Reconciliation(t, &h3ReconciliationAdaptor{})
	require.NoError(t, model.DB.Model(task).Update("submit_time", time.Now().Add(-48*time.Hour).Unix()).Error)
	ordinary := &model.Task{TaskID: "ordinary_timeout", Platform: "ordinary", UserId: 880, ChannelId: 880, Status: model.TaskStatusQueued, Progress: "0%", SubmitTime: time.Now().Add(-24 * time.Hour).Unix()}
	require.NoError(t, model.DB.Create(ordinary).Error)
	// Legacy tasks may have a NULL platform; they remain eligible for cleanup.
	require.NoError(t, model.DB.Model(ordinary).Update("platform", nil).Error)
	// An older H3 reservation must not consume the bounded ordinary sweep.
	timedOut := model.GetTimedOutUnfinishedTasks(time.Now().Add(-time.Minute).Unix(), 1)
	require.Len(t, timedOut, 1)
	assert.Equal(t, ordinary.TaskID, timedOut[0].TaskID)
	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })
	sweepTimedOutTasks(context.Background())
	require.NoError(t, model.DB.First(ordinary, ordinary.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, ordinary.Status)
	assert.EqualValues(t, model.TaskStatusSubmitted, reloadH3Task(t, task).Status)
	assert.Equal(t, 775_000, getUserQuota(t, 880))
	assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))
}

func TestH3ReconciliationLateResponseCannotOverwriteCompletedVideo(t *testing.T) {
	for _, lateStatus := range []string{model.TaskStatusInProgress, model.TaskStatusFailure} {
		t.Run(lateStatus, func(t *testing.T) {
			adaptor := &h3ReconciliationAdaptor{result: &relaycommon.TaskInfo{Status: lateStatus, Progress: "30%", Reason: "late failure"}}
			task := setupH3Reconciliation(t, adaptor)
			adaptor.fetch = func(string, string, *model.Task, string) (*http.Response, error) {
				completed := reloadH3Task(t, task)
				completed.Status = model.TaskStatusSuccess
				completed.Progress = "100%"
				completed.PrivateData.ResultURL = "https://storage.example/durable.mp4"
				require.NoError(t, completed.Update())
				return h3Response(t, 5), nil
			}
			err := SyncH3Task(context.Background(), task)
			if lateStatus == model.TaskStatusFailure {
				require.ErrorIs(t, err, model.ErrH3TaskTerminal)
			} else {
				require.NoError(t, err)
			}
			completed := reloadH3Task(t, task)
			assert.EqualValues(t, model.TaskStatusSuccess, completed.Status)
			assert.Equal(t, "https://storage.example/durable.mp4", completed.PrivateData.ResultURL)
			assert.Equal(t, 775_000, getUserQuota(t, 880))
			assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))
		})
	}
}

type h3ProvenRejection struct{}

func (h3ProvenRejection) Error() string             { return "request rejected before task acceptance" }
func (h3ProvenRejection) DefinitiveRejection() bool { return true }

func TestH3ReconciliationOnlyProvenSubmissionRejectionRefunds(t *testing.T) {
	adaptor := &h3ReconciliationAdaptor{fetch: func(string, string, *model.Task, string) (*http.Response, error) {
		return nil, h3ProvenRejection{}
	}}
	task := setupH3Reconciliation(t, adaptor)
	require.NoError(t, SyncH3Task(context.Background(), task))
	assert.EqualValues(t, model.TaskStatusFailure, reloadH3Task(t, task).Status)
	assert.Equal(t, 1_000_000, getUserQuota(t, 880))
	assert.Equal(t, 800_000, getTokenRemainQuota(t, 880))
}

func TestH3ReservationReplayAndConflictPreserveOriginalCharge(t *testing.T) {
	task := setupH3Reconciliation(t, &h3ReconciliationAdaptor{})
	replay := *task
	replay.ID = 0
	replay.Quota = 450_000
	stored, created, err := model.ReserveH3Task(&replay, "immutable-request-hash")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, 225_000, stored.Quota)
	_, _, err = model.ReserveH3Task(&replay, "different-request-hash")
	assert.ErrorIs(t, err, model.ErrH3IdempotencyConflict)
	assert.Equal(t, 775_000, getUserQuota(t, 880))
	assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))
}

func TestH3ReservationFailsClosedWithCacheOrBatchedAccounting(t *testing.T) {
	for _, mode := range []string{"redis", "batch"} {
		t.Run(mode, func(t *testing.T) {
			task := setupH3Reconciliation(t, &h3ReconciliationAdaptor{})
			replay := *task
			replay.ID = 0
			replay.TaskID = "task_new_unsafe"
			common.RedisEnabled = mode == "redis"
			common.BatchUpdateEnabled = mode == "batch"
			_, _, err := model.ReserveH3Task(&replay, "new-request")
			common.RedisEnabled = false
			common.BatchUpdateEnabled = false
			require.ErrorIs(t, err, model.ErrH3UnsafeBillingMode)
			assert.Equal(t, 775_000, getUserQuota(t, 880))
			assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))
			var count int64
			require.NoError(t, model.DB.Model(&model.H3TaskReservation{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
		})
	}
}

func TestH3TransactionFailureRollsBackReservationAndAllDebits(t *testing.T) {
	task := setupH3Reconciliation(t, &h3ReconciliationAdaptor{})
	replay := *task
	replay.ID = 0
	replay.TaskID = "task_storage_failure"
	const callback = "h3_test:reject_insert"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.Task); ok {
			tx.AddError(errors.New("task insert failed"))
		}
	}))
	t.Cleanup(func() { model.DB.Callback().Create().Remove(callback) })
	_, _, err := model.ReserveH3Task(&replay, "new-request")
	require.Error(t, err)
	assert.Equal(t, 775_000, getUserQuota(t, 880))
	assert.Equal(t, 575_000, getTokenRemainQuota(t, 880))
	var count int64
	require.NoError(t, model.DB.Model(&model.H3TaskReservation{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestH3ConcurrentRefundReturnsCreditExactlyOnce(t *testing.T) {
	task := setupH3Reconciliation(t, &h3ReconciliationAdaptor{})
	var group sync.WaitGroup
	var refunded [2]bool
	var errs [2]error
	for i := range 2 {
		group.Go(func() {
			_, refunded[i], errs[i] = model.RefundH3Task(task.TaskID, "confirmed provider failure")
		})
	}
	group.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.NotEqual(t, refunded[0], refunded[1])
	assert.Equal(t, 1_000_000, getUserQuota(t, 880))
	assert.Equal(t, 800_000, getTokenRemainQuota(t, 880))
}
