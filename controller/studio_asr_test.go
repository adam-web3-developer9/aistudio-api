package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStudioASRDurableDirectTask(t *testing.T) {
	db := setupH3ControllerDB(t)
	settings := config.GlobalConfig.ExportAllConfigs()
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(settings)) })
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_expr": `{"qwen3-asr-0.6b":"u(\"seconds\") * 0.001"}`,
	}))
	var accepted, confirmed atomic.Bool
	var submits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "operator-key", r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == constant.StudioASRJobsPath:
			var request struct {
				RequestKey string `json:"requestKey"`
				Language   string `json:"language"`
				Clips      []struct {
					ID string `json:"id"`
				} `json:"clips"`
			}
			if !assert.NoError(t, common.DecodeJson(r.Body, &request)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, "Chinese", request.Language)
			assert.Len(t, request.Clips, 2)
			var task model.Task
			assert.NoError(t, db.Where("task_id = ?", request.RequestKey).First(&task).Error)
			assert.Equal(t, 1500, task.Quota)
			assert.Equal(t, constant.StudioASRModel, task.Properties.OriginModelName)
			submits.Add(1)
			accepted.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"jobId":"private-origin"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, constant.StudioASRJobsPath+"/by-request-key/"):
			if !accepted.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if confirmed.Load() {
				_, _ = fmt.Fprint(w, `{"id":"private-origin","status":"succeeded","stage":"succeeded","billing":{"unit":"second","requestedSeconds":3},"result":{"transcripts":[{"id":"shot-1","text":"你好"},{"id":"shot-2","text":"再见"}]}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":"private-origin","status":"running","stage":"awaiting_upload","billing":{"unit":"second","requestedSeconds":3},"upload":{"token":"scoped-upload-key","files":[{"id":"shot-1","url":"https://worker.example/upload/1"},{"id":"shot-2","url":"https://worker.example/upload/2"}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == constant.StudioASRJobsPath+"/private-origin/inputs":
			confirmed.Store(true)
			_, _ = fmt.Fprint(w, "{}")
		default:
			t.Errorf("Unexpected Studio request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	body := fmt.Sprintf(`{"model":%q,"clips":[{"id":"shot-1","filename":"one.wav","size":1550,"durationMs":1300,"contentType":"audio/wav","sha256":%q},{"id":"shot-2","filename":"two.wav","size":1600,"durationMs":1500,"contentType":"audio/wav","sha256":%q}]}`,
		constant.StudioASRModel, strings.Repeat("a", 64), strings.Repeat("b", 64))
	c, response := h3ControllerContext(body, server.URL, "same-asr-request")
	RelayStudioASR(c)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "private-origin")
	assert.Contains(t, response.Body.String(), "scoped-upload-key")
	var projection struct{ ID string }
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &projection))
	require.NotEmpty(t, projection.ID)
	c, response = h3ControllerContext(body, server.URL, "same-asr-request")
	RelayStudioASR(c)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), projection.ID)
	assert.EqualValues(t, 1, submits.Load())

	c, response = h3ControllerContext("{}", server.URL, "")
	c.Params = gin.Params{{Key: "task_id", Value: projection.ID}}
	RelayStudioASRInputs(c)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "你好")
	assert.NotContains(t, response.Body.String(), "scoped-upload-key")
	var wallet model.User
	require.NoError(t, db.First(&wallet, 801).Error)
	assert.Equal(t, 2_000_000-1500, wallet.Quota)
	task, exists, err := model.GetByTaskId(801, projection.ID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.EqualValues(t, model.TaskStatusSuccess, task.Status)
	assert.Empty(t, relay.TaskModel2Dto(task).Data)

	c, response = h3ControllerContext("{}", server.URL, "")
	c.Params = gin.Params{{Key: "task_id", Value: projection.ID}}
	require.True(t, relayH3TaskFetch(c))
	assert.Equal(t, http.StatusNotFound, response.Code)
}
