package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel/task/h3studio"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type taskSubmissionTestBilling struct {
	events     *[]string
	settleErr  error
	reserveErr error
	onSettle   func()
	refunds    int
}

func (b *taskSubmissionTestBilling) Settle(int) error {
	*b.events = append(*b.events, "settle")
	if b.onSettle != nil {
		b.onSettle()
	}
	return b.settleErr
}

func (b *taskSubmissionTestBilling) Refund(*gin.Context) {
	*b.events = append(*b.events, "refund")
	b.refunds++
}

func (b *taskSubmissionTestBilling) NeedsRefund() bool        { return b.refunds == 0 }
func (b *taskSubmissionTestBilling) GetPreConsumedQuota() int { return 0 }
func (b *taskSubmissionTestBilling) Reserve(int) error {
	*b.events = append(*b.events, "reserve")
	return b.reserveErr
}

func (b *taskSubmissionTestBilling) ReserveBeforeSubmit(quota int) error { return b.Reserve(quota) }

func TestPresentTaskSubmissionUsesNativePresenterAfterPersistence(t *testing.T) {
	plugin, err := pluginruntime.CompilePlugin(`
export const meta = {apiVersion:1,key:"presenter-test",name:"Presenter",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task",routes:[{method:"POST",path:"/vendor/jobs",type:"submit",decode:"decode",render:"created"}]};
export const native = {decode:function(ctx){return {kind:"submit",model:"model",requestBody:ctx.body.value};},created:function(ctx,task){return {data:{task_id:task.task_id},upstream:task.data};}};
export function buildSubmitRequest(){return {}} export function parseSubmitResponse(){return {taskId:"upstream"}} export function buildQueryRequest(){return {}} export function parseTaskResult(){return {status:"SUCCESS"}}
`, pluginruntime.Options{})
	require.NoError(t, err)
	priceData := types.PriceData{}
	priceData.AddOtherRatio("seconds", 5)
	task := &model.Task{TaskID: "task_public", SubmitTime: 123}
	task.SetData(map[string]any{"task_id": "upstream_private"})
	outcome := &taskSubmissionOutcome{
		Result:    &relay.TaskSubmitResult{},
		Task:      task,
		RelayInfo: &relaycommon.RelayInfo{PriceData: priceData},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/vendor/jobs", strings.NewReader(`{"model":"model"}`))
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Plugin: plugin, Route: plugin.Meta.Routes[0]})
	c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Path: "/vendor/jobs", Method: http.MethodPost, Body: map[string]any{"kind": "json", "value": map[string]any{"model": "model"}}})

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{
		"data":{"task_id":"task_public"},
		"upstream":{"task_id":"upstream_private"}
	}`, recorder.Body.String())
	assert.JSONEq(t, `{"seconds":5}`, recorder.Header().Get("X-New-Api-Other-Ratios"))
}

func TestPresentTaskSubmissionFallbackUsesPersistedPublicID(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	outcome := &taskSubmissionOutcome{
		Result:    &relay.TaskSubmitResult{},
		Task:      &model.Task{TaskID: "task_persisted", SubmitTime: 456},
		RelayInfo: &relaycommon.RelayInfo{OriginModelName: "video-model"},
	}

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{
		"id":"task_persisted",
		"task_id":"task_persisted",
		"status":"queued",
		"model":"video-model",
		"created_at":456
	}`, recorder.Body.String())
}

func TestPresentTaskSubmissionUsesHostOpenAIVideoCreateReceipt(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{
		Protocol:  "openai_video",
		Operation: pluginruntime.HostProtocolOperation{Name: "create"},
	})
	task := &model.Task{
		TaskID:     "task_public",
		Status:     model.TaskStatusSubmitted,
		Progress:   "0%",
		CreatedAt:  456,
		Properties: model.Properties{OriginModelName: "video-model"},
	}
	outcome := &taskSubmissionOutcome{Result: &relay.TaskSubmitResult{}, Task: task, RelayInfo: &relaycommon.RelayInfo{}}

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{"id":"task_public","object":"video","model":"video-model","status":"queued","progress":0,"created_at":456}`, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "task_id")
}

func TestExecuteTaskSubmissionRefundsWhenInsertFails(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, false, &events)
	_ = database
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "task_insert_failed", taskErr.Code)
	assert.Equal(t, []string{"reserve", "insert", "refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionSettlementFailureStaysDurableAndWritesNothing(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events, settleErr: errors.New("settlement failed")}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "task_billing_settlement_failed", taskErr.Code)
	assert.Equal(t, []string{"reserve", "insert", "settle"}, events)
	assert.Zero(t, billing.refunds)
	var count int64
	require.NoError(t, database.Model(&model.Task{}).Where("task_id = ?", "task_public").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionPersistsPinnedPluginProvenance(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, true, &events)
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

	c := taskSubmissionTestContext()
	c.Set(common.RequestIdKey, "request-public")
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{
		Generation: &pluginruntime.RoutingGeneration{Number: 42},
		Plugin: &pluginruntime.LoadedPlugin{Meta: pluginruntime.Meta{
			Key:        "document-parser",
			Name:       "Document Parser",
			Version:    "1.2.3",
			APIVersion: 1,
			Author: pluginruntime.AuthorMeta{
				Name: "Community Author",
				URL:  "https://plugins.example/author",
			},
		}},
	})
	billing := &taskSubmissionTestBilling{events: &events}
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream-private",
			Platform:       constant.TaskPlatform("document-parser"),
		}, nil
	})

	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	require.NotNil(t, outcome.Task.PrivateData.Execution)
	require.NotNil(t, outcome.Task.PrivateData.Execution.TaskPlugin)
	assert.Equal(t, "request-public", outcome.Task.PrivateData.Execution.RequestID)
	assert.Equal(t, "/plugin/submit", outcome.Task.PrivateData.Execution.RequestPath)
	assert.Equal(t, "1.2.3", outcome.Task.PrivateData.Execution.TaskPlugin.Version)
	assert.Equal(t, uint64(42), outcome.Task.PrivateData.Execution.TaskPlugin.Generation)
	require.NotNil(t, outcome.Task.PrivateData.Execution.TaskPlugin.Author)
	assert.Equal(t, "Community Author", outcome.Task.PrivateData.Execution.TaskPlugin.Author.Name)
	assert.Equal(t, "https://plugins.example/author", outcome.Task.PrivateData.Execution.TaskPlugin.Author.URL)

	var stored model.Task
	require.NoError(t, database.Where("task_id = ?", "task_public").First(&stored).Error)
	require.NotNil(t, stored.PrivateData.Execution)
	require.NotNil(t, stored.PrivateData.Execution.TaskPlugin)
	assert.Equal(t, "document-parser", stored.PrivateData.Execution.TaskPlugin.Key)
	require.NotNil(t, stored.PrivateData.Execution.TaskPlugin.Author)
	assert.Equal(t, "Community Author", stored.PrivateData.Execution.TaskPlugin.Author.Name)
	assert.Equal(t, "upstream-private", stored.PrivateData.UpstreamTaskID)
}

// A submit route declaring retainResult: false persists the task row for
// billing but never writes the upstream snapshot for an immediate terminal
// result, while the in-memory task still carries it for the presenter. An
// asynchronous result on the same route is retained because polling and
// retrieval depend on it.
func TestExecuteTaskSubmissionHonorsRouteRetainResult(t *testing.T) {
	retainFalse := false
	for _, tc := range []struct {
		name          string
		immediate     *relaycommon.TaskInfo
		wantDiscarded bool
	}{
		{"immediate success is discarded", &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, Progress: "100%"}, true},
		{"immediate failure is discarded", &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "rejected"}, true},
		{"asynchronous result is retained", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make([]string, 0, 3)
			database, _ := openTaskDialectDatabase(t, &model.Task{}, &model.User{}, &model.Channel{})
			previousDB := model.DB
			model.DB = database
			t.Cleanup(func() { model.DB = previousDB })
			previousLogConsumeEnabled := common.LogConsumeEnabled
			common.LogConsumeEnabled = false
			t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

			c := taskSubmissionTestContext()
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
				Plugin: &pluginruntime.LoadedPlugin{Meta: pluginruntime.Meta{Key: "sync-images"}},
				Route:  pluginruntime.Route{Method: http.MethodPost, Path: "/sync/images", Type: pluginruntime.RouteTypeSubmit, RetainResult: &retainFalse},
			})
			info := taskSubmissionRelayInfo(&taskSubmissionTestBilling{events: &events})
			upstream := []byte(`{"data":[{"url":"https://cdn.example/a.png"}]}`)

			outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
				return &relay.TaskSubmitResult{
					UpstreamTaskID: "task_public",
					Platform:       constant.TaskPlatform("sync-images"),
					TaskData:       upstream,
					Immediate:      tc.immediate,
				}, nil
			})
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			assert.JSONEq(t, string(upstream), string(outcome.Task.Data), "presenter keeps the in-memory snapshot")
			assert.Equal(t, tc.wantDiscarded, outcome.Task.PrivateData.ResultDiscarded)
			assert.Equal(t, !tc.wantDiscarded, outcome.Task.ResultRetrievable())

			var stored model.Task
			require.NoError(t, database.Where("task_id = ?", "task_public").First(&stored).Error)
			assert.Equal(t, tc.wantDiscarded, stored.PrivateData.ResultDiscarded)
			var nullCount int64
			require.NoError(t, database.Model(&model.Task{}).Where("task_id = ? AND data IS NULL", "task_public").Count(&nullCount).Error)
			if tc.wantDiscarded {
				assert.Equal(t, int64(1), nullCount, "snapshot column must stay NULL")
				artifacts, err := projectTaskArtifacts(&stored)
				require.NoError(t, err)
				assert.Empty(t, artifacts)
			} else {
				assert.Equal(t, int64(0), nullCount)
				assert.JSONEq(t, string(upstream), string(stored.Data))
			}
			listed := model.TaskGetAllUserTask(1, 0, 10, model.SyncTaskQueryParams{})
			require.Len(t, listed, 1)
			assert.Empty(t, listed[0].Data, "task lists never select the snapshot column")
		})
	}
}

func TestExecuteTaskSubmissionRefundsCancellationBeforeDurableBarrier(t *testing.T) {
	events := make([]string, 0, 2)
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		cancel()
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "request_cancelled", taskErr.Code)
	assert.Equal(t, []string{"refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionDisconnectBeforeUpstreamAcceptanceSkipsSubmitAndRefunds(t *testing.T) {
	events := make([]string, 0, 1)
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(requestContext)
	info := taskSubmissionRelayInfo(billing)
	submitted := false

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		submitted = true
		return nil, nil
	})

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "request_cancelled", taskErr.Code)
	assert.False(t, submitted)
	assert.Equal(t, []string{"refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionCallerCancellationDuringSubmitRefundsBeforeDurableBarrier(t *testing.T) {
	events := make([]string, 0, 1)
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events}
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	info := taskSubmissionRelayInfo(billing)
	submitStarted := make(chan struct{})
	done := make(chan struct{})
	var outcome *taskSubmissionOutcome
	var taskErr *dto.TaskError

	go func() {
		defer close(done)
		outcome, taskErr = executeTaskSubmissionWith(c, info, func(c *gin.Context, _ *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
			close(submitStarted)
			<-c.Request.Context().Done()
			return nil, service.TaskErrorWrapperLocal(c.Request.Context().Err(), "do_request_failed", http.StatusInternalServerError)
		})
	}()
	select {
	case <-submitStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "submission did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "submission did not stop after disconnect")
	}

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "request_cancelled", taskErr.Code)
	assert.Equal(t, []string{"refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionDisconnectAfterDurableInsertDoesNotRefund(t *testing.T) {
	events := make([]string, 0, 3)
	database := setupTaskSubmissionDatabase(t, true, &events)
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })
	c := taskSubmissionTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	billing := &taskSubmissionTestBilling{
		events:   &events,
		onSettle: cancel,
	}
	info := taskSubmissionRelayInfo(billing)

	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{
			UpstreamTaskID: "upstream_private",
			Platform:       constant.TaskPlatform("plugin"),
		}, nil
	})

	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	assert.Equal(t, "task_public", outcome.Task.TaskID)
	assert.Equal(t, []string{"reserve", "insert", "settle"}, events)
	assert.Zero(t, billing.refunds)
	var count int64
	require.NoError(t, database.Model(&model.Task{}).Where("task_id = ?", "task_public").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	assert.False(t, c.Writer.Written())
}

// setupTaskSubmissionDatabase opens the dialect selected by
// TEST_TASK_DB_DIALECT (SQLite in memory by default) and records every task
// INSERT in events so tests can assert the reserve → insert → settle order.
// Without migrate the task table does not exist and inserts fail.
func setupTaskSubmissionDatabase(t *testing.T, migrate bool, events *[]string) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	var models []any
	if migrate {
		models = append(models, &model.Task{})
	}
	database, _ := openTaskDialectDatabase(t, models...)
	require.NoError(t, database.Callback().Create().Before("gorm:create").Register("test:task-submit-order", func(*gorm.DB) {
		*events = append(*events, "insert")
	}))
	model.DB = database
	t.Cleanup(func() { model.DB = previousDB })
	return database
}

func taskSubmissionTestContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/plugin/submit", strings.NewReader(`{}`))
	return c
}

func taskSubmissionRelayInfo(billing relaycommon.BillingSettler) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		UserId:          1,
		UsingGroup:      "default",
		OriginModelName: "plugin-model",
		Billing:         billing,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:  "task_public",
			LockedChannel: &model.Channel{Id: 1, Type: constant.ChannelTypeTaskPlugin, Name: "plugin"},
		},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1, ChannelType: constant.ChannelTypeTaskPlugin},
	}
}

// Uses the real submit adaptor, expression evaluator, BillingSession, task row
// and consume log. Set TEST_TASK_DB_DIALECT plus TEST_MYSQL_DSN or
// TEST_POSTGRES_DSN to exercise the same contract on an external test database.
// Unique table prefixes keep the fixture isolated from all existing tables.
// openTaskDialectDatabase opens the engine selected by TEST_TASK_DB_DIALECT
// (default SQLite in memory; MySQL and PostgreSQL through TEST_MYSQL_DSN and
// TEST_POSTGRES_DSN) with a unique table prefix, migrates the given models and
// drops them on cleanup. It logs the engine version so database verification
// runs leave a record.
func openTaskDialectDatabase(t *testing.T, models ...any) (*gorm.DB, common.DatabaseType) {
	t.Helper()
	dialect := common.DatabaseType(os.Getenv("TEST_TASK_DB_DIALECT"))
	var driver gorm.Dialector
	switch dialect {
	case "", common.DatabaseTypeSQLite:
		dialect = common.DatabaseTypeSQLite
		driver = sqlite.Open(":memory:")
	case common.DatabaseTypeMySQL:
		require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
		driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
	case common.DatabaseTypePostgreSQL:
		require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
		driver = postgres.New(postgres.Config{DSN: os.Getenv("TEST_POSTGRES_DSN"), PreferSimpleProtocol: true})
	default:
		t.Fatalf("unsupported test dialect %q", dialect)
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("tsubmit_%d_", time.Now().UnixNano())}})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(models...))
	t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(models...)) })
	var version string
	if dialect == common.DatabaseTypeSQLite {
		require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&version).Error)
	} else {
		require.NoError(t, db.Raw("SELECT version()").Scan(&version).Error)
	}
	t.Logf("database: %s %s", dialect, version)
	return db, dialect
}

func TestImmediateTaskSettlementDatabase(t *testing.T) {
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.Channel{}, &model.Task{}, &model.Log{})
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	oldRedis, oldMemory, oldBatch, oldConsume, oldExport := common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dialect, dialect)
	common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = false, false, false, true, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
		common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.DataExportEnabled = oldRedis, oldMemory, oldBatch, oldConsume, oldExport
	})

	const expression = `u("units") == 7 ? tier("missing", u("missing") * 1.0) : u("units") == 8 ? tier("invalid", -1.0) : u("units") > 3 ? tier("bulk", u("units") * 0.01) : tier("small", u("units") * 0.01)`
	withTieredBillingConfig(t, map[string]string{"document-model": "tiered_expr"}, map[string]string{"document-model": expression})
	const source = `
export const meta={apiVersion:1,key:"generic-settlement",name:"Generic settlement",version:"1.0.0",author:{name:"Test"},models:["document-model"],fetchMode:"per_task",usageSchema:{units:{type:"number",unit:"count"}}};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/compile",body:ctx.requestBody};}
export function parseSubmitResponse(ctx,resp){return {taskId:"vendor-job",taskData:resp.body,immediate:{status:resp.body.status,reason:"provider rejected job"}};}
export function extractUsage(){return {units:4};}
export function extractUsageOnComplete(ctx,result,body){return body.usage;}
export function parseTaskResult(){throw new Error("completed submissions must not poll");}
export function buildQueryRequest(){throw new Error("completed submissions must not poll");}
`
	plugin, err := pluginruntime.CompilePlugin(source, pluginruntime.Options{})
	require.NoError(t, err)
	for index, tc := range []struct {
		name, status string
		actual       any
		count        float64
	}{
		{"partial", "SUCCESS", 2, 2}, {"zero", "SUCCESS", 0, 0}, {"larger", "SUCCESS", 6, 6},
		{"invalid usage", "SUCCESS", -1, 4}, {"expression failure", "SUCCESS", 7, 4}, {"negative result", "SUCCESS", 8, 4}, {"missing usage", "SUCCESS", nil, 4}, {"failed", "FAILURE", 9, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := map[string]any{"status": tc.status}
				if tc.actual != nil {
					body["usage"] = map[string]any{"units": tc.actual}
				}
				encoded, err := common.Marshal(body)
				if err != nil {
					panic(err)
				}
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			initial := int(20 * common.QuotaPerUnit)
			user := model.User{Username: fmt.Sprintf("task_user_%d", index), AffCode: fmt.Sprintf("task_aff_%d", index), Quota: initial}
			require.NoError(t, db.Create(&user).Error)
			ch := model.Channel{Name: "test provider", Type: constant.ChannelTypeTaskPlugin}
			require.NoError(t, db.Create(&ch).Error)
			c := taskSubmissionTestContext()
			c.Set("group", "default")
			c.Set("username", user.Username)
			c.Set("task_request", map[string]any{"model": "document-model"})
			c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "document-model")
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
			common.SetContextKey(c, constant.ContextKeyChannelId, ch.Id)
			common.SetContextKey(c, constant.ContextKeyChannelType, ch.Type)
			info := taskSubmissionRelayInfo(nil)
			info.UserId = user.Id
			info.OriginModelName = "document-model"
			info.UserGroup = "default"
			info.UsingGroup = "default"
			info.IsPlayground = true
			info.UserSetting.BillingPreference = "wallet_only"
			info.PublicTaskID = model.GenerateTaskID()
			info.LockedChannel = &ch
			outcome, taskErr := executeTaskSubmission(c, info)
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			want := common.QuotaRound(tc.count * 0.01 * common.QuotaPerUnit)
			assert.Equal(t, want, outcome.Result.Quota)
			assert.Equal(t, want, info.PriceData.Quota)
			var stored model.Task
			require.NoError(t, db.Where("task_id = ?", info.PublicTaskID).First(&stored).Error)
			assert.Equal(t, want, stored.Quota)
			assert.Equal(t, model.TaskStatus(tc.status), stored.Status)
			assert.Positive(t, stored.FinishTime)
			assert.Equal(t, float64(4), info.TieredBillingSnapshot.EstimatedQuotaBeforeGroup/(0.01*common.QuotaPerUnit))
			if tc.status == "SUCCESS" {
				assert.Equal(t, tc.count, stored.PrivateData.BillingContext.TieredSnapshot.UsageFacts["units"])
			}
			var updated model.User
			require.NoError(t, db.First(&updated, user.Id).Error)
			assert.Equal(t, initial-want, updated.Quota)
			assert.Equal(t, want, updated.UsedQuota)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ?", user.Id).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, want, logs[0].Quota)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			if tc.status == "SUCCESS" {
				assert.Equal(t, tc.count, other["usage_facts"].(map[string]any)["units"])
			}
			assert.False(t, c.Writer.Written(), "presentation must follow persistence and settlement")
			require.NoError(t, info.Billing.Settle(want))
			info.Billing.Refund(c)
			require.NoError(t, db.First(&updated, user.Id).Error)
			assert.Equal(t, initial-want, updated.Quota, "terminal settlement is idempotent")
		})
	}
}

func TestAcceptedSubmitStreamNeverRetries(t *testing.T) {
	c := taskSubmissionTestContext()
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "task_accepted", Source: "system"}, decideTaskRetry(c, &dto.TaskError{StatusCode: 502, LocalError: true, NoRetry: true}, 3))
}

// Local task rejections carry a message but no cause; the response and the
// decision record must still be produced.
func TestRespondTaskSubmissionErrorWithoutCause(t *testing.T) {
	previousErrorLog := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = false
	t.Cleanup(func() { constant.ErrorLogEnabled = previousErrorLog })
	c := taskSubmissionTestContext()
	taskErr := &dto.TaskError{Code: "get_channel_failed", Message: "no channel", StatusCode: http.StatusServiceUnavailable, LocalError: true}
	require.NotPanics(t, func() { respondTaskSubmissionError(c, taskErr) })
	assert.Equal(t, http.StatusServiceUnavailable, c.Writer.Status())
	events := service.RequestPolicy(c).Events()
	require.Len(t, events, 1)
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "request_failed", Source: "system"}, events[0].Decision)
	assert.Equal(t, http.StatusServiceUnavailable, events[0].Status)
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "local_rejection", Source: "system"}, decideTaskRetry(c, &dto.TaskError{StatusCode: http.StatusForbidden, LocalError: true, Message: "billing"}, 2), "local errors stop once the status rules do not force a retry")
}

func TestExecuteTaskSubmissionRefundsWhenFinalReserveFails(t *testing.T) {
	events := []string{}
	setupTaskSubmissionDatabase(t, true, &events)
	billing := &taskSubmissionTestBilling{events: &events, reserveErr: errors.New("insufficient funds")}
	c := taskSubmissionTestContext()
	info := taskSubmissionRelayInfo(billing)
	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{Platform: "plugin", Quota: 600, Immediate: &relaycommon.TaskInfo{Status: "SUCCESS"}}, nil
	})
	require.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, http.StatusForbidden, taskErr.StatusCode)
	assert.Equal(t, []string{"reserve", "refund"}, events)
	assert.Equal(t, 1, billing.refunds)
	assert.False(t, c.Writer.Written())
}

func h3RequestContextForTest(body string) (*gin.Context, *relaycommon.RelayInfo) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_persisted"}}
}

func TestH3RequestFreezesBilledSecondsAndGenerationParameters(t *testing.T) {
	c, info := h3RequestContextForTest(`{"model":"h3","prompt":"a moving cloud","seconds":"10","duration":10,"size":"768x1344","metadata":{"direct_media":{"image1":{"filename":"image1.png","contentType":"image/png","sha256":"` + strings.Repeat("a", 64) + `"}}}}`)
	a := &h3studio.TaskAdaptor{}
	require.Nil(t, a.ValidateRequestAndSetAction(c, info))
	assert.Equal(t, map[string]float64{"seconds": 10}, a.EstimateBilling(c, info))
	reader, err := a.BuildRequestBody(c, info)
	require.NoError(t, err)
	var request h3studio.StudioRequest
	require.NoError(t, common.DecodeJson(reader, &request))
	assert.Equal(t, "task_persisted", request.RequestKey)
	assert.Equal(t, "archive", request.ResultDelivery)
	assert.Equal(t, "direct", request.Transport)
	assert.Equal(t, float64(10), request.Values["duration"])
	assert.Equal(t, float64(768), request.Values["width"])
	assert.Equal(t, float64(1344), request.Values["height"])
	assert.Equal(t, "image1.png", request.DirectMedia["image1"].Filename)
	assert.Empty(t, request.Media)
}

func TestH3RequestWithoutReferencesSendsExplicitEmptyMedia(t *testing.T) {
	c, info := h3RequestContextForTest(`{"model":"h3","prompt":"clouds"}`)
	adaptor := &h3studio.TaskAdaptor{}
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	var request h3studio.StudioRequest
	require.NoError(t, common.DecodeJson(body, &request))
	assert.NotNil(t, request.DirectMedia)
	assert.Empty(t, request.DirectMedia)
	assert.NotNil(t, request.Media)
	assert.Empty(t, request.Media)
	assert.Equal(t, float64(5), request.Values["duration"])
	assert.Equal(t, "direct", request.Transport)
	assert.Equal(t, "archive", request.ResultDelivery)
}

func TestH3InvalidChannelConfigurationDoesNotReserveOrDebit(t *testing.T) {
	database := setupH3ControllerDB(t)
	for _, tc := range []struct{ name, baseURL, credential string }{
		{"missing URL", "", "operator-key"},
		{"invalid scheme", "file:///tmp/private", "operator-key"},
		{"missing host", "https:///jobs", "operator-key"},
		{"query in base URL", "https://studio.example/?key=private", "operator-key"},
		{"missing credential", "https://studio.example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, response := h3ControllerContext(`{"model":"h3","prompt":"clouds"}`, tc.baseURL, "bad-config")
			common.SetContextKey(c, constant.ContextKeyChannelKey, tc.credential)
			RelayH3Task(c)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.NotContains(t, response.Body.String(), "operator-key")
			var reservations int64
			require.NoError(t, database.Model(&model.H3TaskReservation{}).Count(&reservations).Error)
			assert.Zero(t, reservations)
			var user model.User
			require.NoError(t, database.First(&user, 801).Error)
			assert.Equal(t, 2_000_000, user.Quota)
		})
	}
}

func TestH3RequestRejectsBillingBypassesAndUnsupportedMedia(t *testing.T) {
	cases := map[string]string{
		"below minimum":        `"duration":3`,
		"above maximum":        `"duration":16`,
		"overflow":             `"seconds":"18446744073709551616"`,
		"negative":             `"duration":-1`,
		"fractional":           `"duration":5.2`,
		"null":                 `"duration":null`,
		"invalid string":       `"duration":"no"`,
		"conflicting aliases":  `"duration":5,"seconds":"10"`,
		"metadata duration":    `"metadata":{"duration":15}`,
		"metadata values":      `"metadata":{"values":{"duration":15}}`,
		"oversized dimensions": `"size":"1920x1920"`,
		"unaligned dimensions": `"size":"1280x720"`,
		"invalid dimensions":   `"size":"a x b"`,
		"image url":            `"image":"https://example.com/secret.png"`,
		"invalid binding":      `"metadata":{"direct_media":{"image10":{"filename":"a.png","contentType":"image/png","sha256":"` + strings.Repeat("a", 64) + `"}}}`,
		"wrong media type":     `"metadata":{"direct_media":{"image1":{"filename":"a.wav","contentType":"audio/wav","sha256":"` + strings.Repeat("a", 64) + `"}}}`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			c, info := h3RequestContextForTest(`{"model":"h3","prompt":"clouds",` + field + `}`)
			err := (&h3studio.TaskAdaptor{}).ValidateRequestAndSetAction(c, info)
			require.NotNil(t, err)
			assert.Equal(t, http.StatusBadRequest, err.StatusCode)
		})
	}
}

func TestH3ParseOnlyCompletesArchivedDeliveriesAndProvenFailures(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		status    string
		wantError bool
	}{
		{"queued", `{"id":"job1","status":"queued"}`, model.TaskStatusQueued, false},
		{"gpu only result", `{"id":"job1","status":"succeeded","result":{"videoUrl":"https://gpu.example/view"}}`, model.TaskStatusInProgress, false},
		{"archiving", `{"id":"job1","status":"running","stage":"archiving"}`, model.TaskStatusInProgress, false},
		{"unknown remote failure", `{"id":"job1","status":"failed","error":{"code":"COMFY_REMOTE_UNCONFIRMED","providerTerminal":false}}`, model.TaskStatusInProgress, false},
		{"unproven failure", `{"id":"job1","status":"failed","error":{"code":"UNKNOWN"}}`, model.TaskStatusInProgress, false},
		{"proven render failure", `{"id":"job1","status":"failed","error":{"code":"COMFY_RENDER_FAILED","providerTerminal":true}}`, model.TaskStatusFailure, false},
		{"missing billing", `{"id":"job1","status":"succeeded","result":{"storage":"tos","videoUrl":"https://tos.example/video.mp4"}}`, "", true},
		{"archived success", `{"id":"job1","status":"succeeded","billing":{"unit":"second","requestedSeconds":10},"result":{"storage":"tos","videoUrl":"https://tos.example/video.mp4","expiresAt":1800000000000}}`, model.TaskStatusSuccess, false},
		{"malformed", `{"message":"rate limited"}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := (&h3studio.TaskAdaptor{}).ParseTaskResult(nil, nil, []byte(tc.body))
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.status, result.Status)
			if tc.status == model.TaskStatusSuccess {
				assert.Equal(t, "https://tos.example/video.mp4", result.Url)
			}
		})
	}
}

func TestH3FetchRecoversSamePersistedRequestAfterLostSubmitResponse(t *testing.T) {
	service.InitHttpClient()
	posted := 0
	saved := []byte(`{"adapterId":"comfyui-v1","requestKey":"task_recover","workflowId":"minimax-h3-9plus3","values":{"duration":10}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "operator-key", r.Header.Get("x-api-key"))
		if r.Method == http.MethodGet {
			assert.Equal(t, "/v1/jobs/by-request-key/task_recover", r.URL.Path)
			if posted == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		} else {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/v1/jobs", r.URL.Path)
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			assert.JSONEq(t, string(saved), string(body))
			posted++
			_, _ = io.WriteString(w, `{"jobId":"job-original","status":"queued","statusUrl":"/v1/jobs/job-original","billing":{"unit":"second","requestedSeconds":10}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"job-original","status":"queued"}`)
	}))
	defer server.Close()
	a := &h3studio.TaskAdaptor{}
	for i := 0; i < 2; i++ {
		response, err := a.FetchTask(server.URL, "operator-key", &model.Task{TaskID: "task_recover", PrivateData: model.TaskPrivateData{H3: &model.H3TaskContext{RequestBody: saved}}}, "")
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		_ = response.Body.Close()
		assert.Contains(t, string(body), "job-original")
	}
	assert.Equal(t, 1, posted)
}

func TestH3SubmitAcknowledgementDoesNotWriteBeforeDurableTaskResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"jobId":"private-original-job","status":"queued","statusUrl":"/v1/jobs/private-original-job"}`))}
	parsed, taskErr := (&h3studio.TaskAdaptor{}).ParseResponse(c, response, nil)
	require.Nil(t, taskErr)
	assert.Equal(t, "private-original-job", parsed.UpstreamTaskID)
	assert.Empty(t, recorder.Body.String())
	assert.False(t, c.Writer.Written())
}

func TestH3FetchDistinguishesRejectionFromAmbiguousErrors(t *testing.T) {
	service.InitHttpClient()
	for _, code := range []int{400, 401, 403, 422, 409, 500, 503, -1} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if code == -1 {
					connection, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = connection.Close()
					return
				}
				w.WriteHeader(code)
			}))
			defer server.Close()
			_, err := (&h3studio.TaskAdaptor{}).FetchTask(server.URL, "key", &model.Task{TaskID: "task_reject", PrivateData: model.TaskPrivateData{H3: &model.H3TaskContext{RequestBody: []byte(`{}`)}}}, "")
			require.Error(t, err)
			_, definitive := err.(*h3studio.SubmissionRejectedError)
			assert.Equal(t, code == 400 || code == 401 || code == 403 || code == 422, definitive)
		})
	}
}

func TestH3PublicProjectionExcludesOperationalDetails(t *testing.T) {
	a := &h3studio.TaskAdaptor{}
	failedData, err := a.SanitizeTaskData([]byte(`{"id":"private-job","status":"failed","error":{"code":"FAILED","message":"native-secret-diagnostic"}}`))
	require.NoError(t, err)
	assert.NotContains(t, string(failedData), "native-secret-diagnostic")
	safe, err := a.SanitizeTaskData([]byte(`{"id":"private-job","status":"running","stage":"awaiting_upload","events":[{"key":"secret"}],"progress":{"instance":{"id":"gpu-private"},"nativeAttempts":[{"plannedId":"private-prompt"}]},"upload":{"baseUrl":"https://gpu.example","subfolder":"upload-folder","reservationId":"reservation-current","files":[]}}`))
	require.NoError(t, err)
	assert.NotContains(t, string(safe), "private-job")
	assert.NotContains(t, string(safe), "private-prompt")
	assert.NotContains(t, string(safe), "gpu-private")
	assert.NotContains(t, string(safe), "secret")
	task := &model.Task{TaskID: "task_public", Status: model.TaskStatusInProgress, Data: safe, Properties: model.Properties{OriginModelName: h3studio.ModelName}, PrivateData: model.TaskPrivateData{BillingContext: &model.TaskBillingContext{ModelPrice: h3studio.PricePerSecond, OtherRatios: map[string]float64{"seconds": 10}}}}
	response, err := a.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	var video map[string]any
	require.NoError(t, common.Unmarshal(response, &video))
	assert.Equal(t, "task_public", video["id"])
	assert.Equal(t, "10", video["seconds"])
	assert.Contains(t, string(response), "reservation-current")
	assert.Contains(t, string(response), "awaiting_upload")
	assert.NotContains(t, string(response), "private-job")
}

func TestH3TaskAccessRequiresOwnerAndAllowedModel(t *testing.T) {
	for _, tc := range []struct {
		name           string
		user           int
		limit, allowed bool
		status         int
	}{
		{name: "owner", user: 7},
		{name: "another owner", user: 8, status: http.StatusNotFound},
		{name: "model denied", user: 7, limit: true, status: http.StatusForbidden},
		{name: "model allowed", user: 7, limit: true, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Set("id", tc.user)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, tc.limit)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"h3": tc.allowed})
			task := &model.Task{UserId: 7, Platform: constant.TaskPlatformH3Studio, Properties: model.Properties{OriginModelName: "h3"}}
			assert.Equal(t, tc.status == 0, authorizeH3Task(c, task))
			if tc.status != 0 {
				assert.Equal(t, tc.status, recorder.Code)
			}
		})
	}
}

func setupH3ControllerDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousFactory := service.GetTaskAdaptorFunc
	previousLogEnabled, previousExportEnabled := common.LogConsumeEnabled, common.DataExportEnabled
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	db := setupModelListControllerTestDB(t)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		service.GetTaskAdaptorFunc = previousFactory
		common.LogConsumeEnabled, common.DataExportEnabled = previousLogEnabled, previousExportEnabled
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
	})
	common.LogConsumeEnabled, common.DataExportEnabled = true, false
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Task{}, &model.H3TaskReservation{}, &model.Log{}))
	require.NoError(t, db.Create(&model.User{Id: 801, Username: "h3-api-user", Status: common.UserStatusEnabled, Quota: 2_000_000}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 801, UserId: 801, Key: "h3-client-key", Status: common.TokenStatusEnabled, RemainQuota: 2_000_000, ExpiredTime: -1}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 801, Type: constant.ChannelTypeH3Studio, Key: "operator-key", Status: common.ChannelStatusEnabled}).Error)
	service.InitHttpClient()
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	return db
}

func setupH3ControllerBilling(t *testing.T) (*gorm.DB, string, *int) {
	t.Helper()
	db := setupH3ControllerDB(t)
	accepted := map[string]int{}
	created := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "operator-key", r.Header.Get("x-api-key"))
		var key string
		if r.Method == http.MethodPost {
			var request h3studio.StudioRequest
			if !assert.NoError(t, common.DecodeJson(r.Body, &request)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			key = request.RequestKey
			// The real upstream only sees a request after task and wallet debit
			// are durably committed together.
			var persisted model.Task
			if !assert.NoError(t, db.Where("task_id = ?", key).First(&persisted).Error) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			assert.Positive(t, persisted.Quota)
			var wallet model.User
			var token model.Token
			var reservation model.H3TaskReservation
			assert.NoError(t, db.First(&wallet, persisted.UserId).Error)
			assert.NoError(t, db.First(&token, persisted.PrivateData.TokenId).Error)
			assert.NoError(t, db.Where("task_id = ?", key).First(&reservation).Error)
			assert.Equal(t, 2_000_000-persisted.Quota, wallet.Quota)
			assert.Equal(t, 2_000_000-persisted.Quota, token.RemainQuota)
			assert.Equal(t, persisted.Quota, reservation.Quota)
			assert.Equal(t, "archive", request.ResultDelivery)
			accepted[key] = int(request.Values["duration"].(float64))
			*created++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jobId":"private-upstream-job","status":"queued","statusUrl":"/v1/jobs/private-upstream-job","billing":{"unit":"second","requestedSeconds":%d}}`, accepted[key])
			return
		} else {
			key = strings.TrimPrefix(r.URL.Path, "/v1/jobs/by-request-key/")
			if accepted[key] == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"private-upstream-job","status":"queued","stage":"queued","billing":{"unit":"second","requestedSeconds":%d},"events":[{"owner":"operator-private"}]}`, accepted[key])
	}))
	t.Cleanup(server.Close)
	return db, server.URL, created
}

func h3ControllerContext(body, baseURL, idempotencyKey string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		c.Request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	for key, value := range map[constant.ContextKey]any{
		constant.ContextKeyUserId:         801,
		constant.ContextKeyUserName:       "h3-api-user",
		constant.ContextKeyUserGroup:      "h3-discounted",
		constant.ContextKeyUsingGroup:     "h3-discounted",
		constant.ContextKeyUserQuota:      2_000_000,
		constant.ContextKeyTokenId:        801,
		constant.ContextKeyTokenKey:       "h3-client-key",
		constant.ContextKeyChannelId:      801,
		constant.ContextKeyChannelType:    constant.ChannelTypeH3Studio,
		constant.ContextKeyChannelBaseUrl: baseURL,
		constant.ContextKeyChannelKey:     "operator-key",
		constant.ContextKeyOriginalModel:  "h3",
	} {
		common.SetContextKey(c, key, value)
	}
	return c, recorder
}

func TestH3ControllerBillsRequestedSecondsWithoutTextGroupDiscount(t *testing.T) {
	oldGroups := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroups)) })
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"h3-discounted":0.5}`))
	for _, tc := range []struct{ seconds, quota int }{{5, 225_000}, {10, 450_000}} {
		t.Run(fmt.Sprintf("%d seconds", tc.seconds), func(t *testing.T) {
			db, baseURL, created := setupH3ControllerBilling(t)
			c, response := h3ControllerContext(fmt.Sprintf(`{"model":"h3","prompt":"clouds","seconds":"%d"}`, tc.seconds), baseURL, "")
			RelayTask(c)
			require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
			var video kitdto.OpenAIVideo
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &video))
			assert.Equal(t, fmt.Sprint(tc.seconds), video.Seconds)
			assert.Equal(t, "h3", video.Model)
			assert.NotContains(t, response.Body.String(), "private-upstream-job")
			assert.NotContains(t, response.Body.String(), "operator-private")
			var user model.User
			var token model.Token
			var task model.Task
			require.NoError(t, db.First(&user, 801).Error)
			require.NoError(t, db.First(&token, 801).Error)
			require.NoError(t, db.Where("task_id = ?", video.ID).First(&task).Error)
			assert.Equal(t, 2_000_000-tc.quota, user.Quota)
			assert.Equal(t, 2_000_000-tc.quota, token.RemainQuota)
			assert.Equal(t, tc.quota, task.Quota)
			assert.Equal(t, 1.0, task.PrivateData.BillingContext.GroupRatio)
			assert.Equal(t, 1, *created)
		})
	}
}

func TestH3ControllerIdempotencyChargesOnceAndRejectsChangedRequest(t *testing.T) {
	db, baseURL, created := setupH3ControllerBilling(t)
	var firstTaskID string
	for i := 0; i < 2; i++ {
		c, response := h3ControllerContext(`{"model":"h3","prompt":"clouds","seconds":"5"}`, baseURL, "client-generation-1")
		RelayH3Task(c)
		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
		var video kitdto.OpenAIVideo
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &video))
		if i == 0 {
			firstTaskID = video.ID
		}
		assert.Equal(t, firstTaskID, video.ID)
	}
	c, response := h3ControllerContext(`{"model":"h3","prompt":"a changed prompt","seconds":"5"}`, baseURL, "client-generation-1")
	RelayH3Task(c)
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	var user model.User
	require.NoError(t, db.First(&user, 801).Error)
	assert.Equal(t, 1_775_000, user.Quota)
	assert.Equal(t, 1, user.RequestCount)
	var count int64
	require.NoError(t, db.Model(&model.H3TaskReservation{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	assert.Equal(t, 1, *created)
}

func TestH3UploadReceiptRejectsOtherUsersAndTokenModelRestrictions(t *testing.T) {
	_, baseURL, _ := setupH3ControllerBilling(t)
	c, response := h3ControllerContext(`{"model":"h3","prompt":"clouds","seconds":"5"}`, baseURL, "owner-task")
	RelayH3Task(c)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	var video kitdto.OpenAIVideo
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &video))
	for _, tc := range []struct {
		name         string
		user, status int
		limited      bool
	}{
		{"other user", 802, http.StatusNotFound, false},
		{"token without H3", 801, http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := h3ControllerContext(`{"reservationId":"current","inputs":{}}`, baseURL, "")
			c.Params = gin.Params{{Key: "video_id", Value: video.ID}}
			common.SetContextKey(c, constant.ContextKeyUserId, tc.user)
			if tc.limited {
				common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
				common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"text-model": true})
			}
			RelayH3Inputs(c)
			assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
		})
	}
}

func TestH3UploadReceiptForwardsReservationAndRefreshesPublicTask(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stale         bool
		topLevelError bool
	}{
		{name: "accepted"},
		{name: "stale nested error", stale: true},
		{name: "stale Fastify error", stale: true, topLevelError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := tc.stale
			db := setupH3ControllerDB(t)
			accepted, receivedInputs := false, false
			lookupsAfterInputs := 0
			reservation := "current-reservation"
			if stale {
				reservation = "stale-reservation"
			}
			receipt := `{"reservationId":"` + reservation + `", "inputs":{"image1":{"name":"image1.png","subfolder":"upload-job-original","type":"input"}}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "operator-key", r.Header.Get("x-api-key"))
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
					accepted = true
					_, _ = io.WriteString(w, `{"jobId":"upload-job-original","status":"queued","statusUrl":"/v1/jobs/upload-job-original","billing":{"unit":"second","requestedSeconds":5}}`)
					return
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/by-request-key/task_"):
					if !accepted {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if receivedInputs {
						lookupsAfterInputs++
					}
				case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/upload-job-original/inputs":
					body, err := io.ReadAll(r.Body)
					if !assert.NoError(t, err) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					assert.Equal(t, receipt, string(body), "the reservation-bound receipt must be forwarded unchanged")
					receivedInputs = true
					if stale {
						w.WriteHeader(http.StatusConflict)
						if tc.topLevelError {
							_, _ = io.WriteString(w, `{"statusCode":409,"error":"Conflict","code":"COMFY_UPLOAD_RESERVATION_STALE","message":"internal diagnostic operator-key"}`)
							return
						}
						_, _ = io.WriteString(w, `{"error":{"code":"COMFY_UPLOAD_RESERVATION_STALE","message":"internal diagnostic operator-key"}}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"upload-job-original","status":"running","stage":"preparing"}`)
					return
				default:
					assert.Fail(t, "unexpected Studio request", "%s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if receivedInputs && !stale {
					_, _ = io.WriteString(w, `{"id":"upload-job-original","status":"running","stage":"preparing","billing":{"unit":"second","requestedSeconds":5}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"upload-job-original","status":"running","stage":"awaiting_upload","billing":{"unit":"second","requestedSeconds":5},"upload":{"baseUrl":"https://gpu.example","subfolder":"upload-job-original","reservationId":"current-reservation","files":[{"binding":"image1","filename":"image1.png","contentType":"image/png","sha256":"`+strings.Repeat("a", 64)+`"}]},"events":[{"credential":"operator-key"}]}`)
			}))
			defer server.Close()
			request := `{"model":"h3","prompt":"animate this image","seconds":"5","metadata":{"direct_media":{"image1":{"filename":"image1.png","contentType":"image/png","sha256":"` + strings.Repeat("a", 64) + `"}}}}`
			createContext, createResponse := h3ControllerContext(request, server.URL, "upload-end-to-end")
			RelayH3Task(createContext)
			require.Equal(t, http.StatusAccepted, createResponse.Code, createResponse.Body.String())
			var createdVideo kitdto.OpenAIVideo
			require.NoError(t, common.Unmarshal(createResponse.Body.Bytes(), &createdVideo))
			upload, ok := createdVideo.Metadata["upload"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "https://gpu.example", upload["baseUrl"])
			assert.Equal(t, "current-reservation", upload["reservationId"])
			assert.Equal(t, "upload-job-original", upload["subfolder"])
			assert.NotContains(t, createResponse.Body.String(), "operator-key")
			inputContext, inputResponse := h3ControllerContext(receipt, server.URL, "")
			router := gin.New()
			router.POST("/v1/videos/:video_id/inputs", func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUserId, 801)
			}, RelayH3Inputs)
			inputContext.Request.URL.Path = "/v1/videos/" + createdVideo.ID + "/inputs"
			router.ServeHTTP(inputResponse, inputContext.Request)
			assert.True(t, receivedInputs)
			assert.NotContains(t, inputResponse.Body.String(), "operator-key")
			if stale {
				assert.Equal(t, http.StatusConflict, inputResponse.Code, inputResponse.Body.String())
				var failure struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, common.Unmarshal(inputResponse.Body.Bytes(), &failure))
				assert.Equal(t, "COMFY_UPLOAD_RESERVATION_STALE", failure.Error.Code)
				assert.Zero(t, lookupsAfterInputs)
			} else {
				require.Equal(t, http.StatusOK, inputResponse.Code, inputResponse.Body.String())
				var refreshed kitdto.OpenAIVideo
				require.NoError(t, common.Unmarshal(inputResponse.Body.Bytes(), &refreshed))
				assert.Equal(t, createdVideo.ID, refreshed.ID)
				assert.Equal(t, "preparing", refreshed.Metadata["stage"])
				assert.NotContains(t, refreshed.Metadata, "upload")
				assert.Equal(t, 1, lookupsAfterInputs)
			}
			var user model.User
			require.NoError(t, db.First(&user, 801).Error)
			assert.Equal(t, 1_775_000, user.Quota, "upload acknowledgements never charge or refund the video")
			assert.Equal(t, 1, user.RequestCount)
		})
	}
}
