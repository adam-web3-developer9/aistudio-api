package controller

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay/channel/task/h3studio"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

// RelayH3Task uses a durable reservation rather than the generic task submit
// retry loop: losing a Studio response must never refund a running GPU job.
func RelayH3Task(c *gin.Context) {
	submitStudioTask(c, h3studio.ModelName)
}

func RelayStudioASR(c *gin.Context) {
	if c.GetInt("channel_type") != constant.ChannelTypeH3Studio {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "This endpoint requires a Studio speech recognition channel")
		return
	}
	submitStudioTask(c, constant.StudioASRModel)
}

func submitStudioTask(c *gin.Context, modelName string) {
	isASR := modelName == constant.StudioASRModel
	mediaType := "video"
	if isASR {
		mediaType = "speech recognition"
	}
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to prepare "+mediaType+" request")
		return
	}
	info.InitChannelMeta(c)
	upstreamURL, configErr := url.Parse(info.ChannelBaseUrl)
	if configErr != nil || upstreamURL == nil || upstreamURL.Host == "" ||
		(upstreamURL.Scheme != "http" && upstreamURL.Scheme != "https") ||
		upstreamURL.User != nil || upstreamURL.RawQuery != "" || upstreamURL.Fragment != "" || strings.TrimSpace(info.ApiKey) == "" {
		videoProxyError(c, http.StatusServiceUnavailable, "configuration_error", "Studio channel requires a valid HTTP(S) base URL and operator credential")
		return
	}
	if strings.HasSuffix(c.Request.URL.Path, "/remix") {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "H3 remix is not supported")
		return
	}
	if common.NormalizeBillingPreference(info.UserSetting.BillingPreference) == "subscription_only" {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "Studio "+mediaType+" uses wallet credit; select a billing preference that permits wallet charges")
		return
	}
	info.PublicTaskID = model.GenerateTaskID()
	if idempotencyKey := c.GetHeader("Idempotency-Key"); idempotencyKey != "" {
		if len(idempotencyKey) > 128 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
			videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key must contain 1 to 128 characters without surrounding whitespace")
			return
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%s", modelName, info.UserId, info.TokenId, idempotencyKey)))
		info.PublicTaskID = fmt.Sprintf("task_%x", sum)
	}
	var body []byte
	var seconds float64
	var size string
	upstreamModel := h3studio.WorkflowID
	if isASR {
		body, seconds, err = h3studio.BuildASRRequest(c, info.PublicTaskID)
		if err != nil {
			videoProxyError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		info.Action = constant.TaskActionTranscription
		upstreamModel = modelName
	} else {
		adaptor := &h3studio.TaskAdaptor{}
		adaptor.Init(info)
		if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
			respondTaskError(c, taskErr)
			return
		}
		bodyReader, err := adaptor.BuildRequestBody(c, info)
		if err != nil {
			videoProxyError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		body, err = io.ReadAll(io.LimitReader(bodyReader, 1024*1024+1))
		if err != nil || len(body) > 1024*1024 {
			videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "H3 metadata exceeds the request limit")
			return
		}
		seconds = adaptor.EstimateBilling(c, info)["seconds"]
		if seconds < 4 || seconds > 15 {
			videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "H3 duration must be between 4 and 15 seconds")
			return
		}
		validatedRequest, _ := relaycommon.GetTaskRequest(c)
		size = validatedRequest.Size
	}
	expression, configured := billing_setting.GetBillingExpr(modelName)
	if !configured {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Invalid Studio "+mediaType+" billing configuration")
		return
	}
	cost, _, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: map[string]any{"seconds": seconds}})
	if err != nil {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Invalid Studio "+mediaType+" billing configuration")
		return
	}
	pricePerSecond := cost / seconds
	quota, clamp := common.QuotaRoundChecked(cost * common.QuotaPerUnit)
	if clamp != nil || quota <= 0 {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Invalid Studio "+mediaType+" billing configuration")
		return
	}
	task := model.InitTask(constant.TaskPlatformH3Studio, info)
	task.Status = model.TaskStatusSubmitted
	task.Action = info.Action
	task.Quota = quota
	task.Properties.OriginModelName = modelName
	task.Properties.UpstreamModelName = upstreamModel
	task.PrivateData.Key = info.ApiKey
	task.PrivateData.TokenId = info.TokenId
	task.PrivateData.NodeName = common.NodeName
	task.PrivateData.BillingSource = service.BillingSourceWallet
	task.PrivateData.H3 = &model.H3TaskContext{RequestBody: body, BillingExpr: expression, Seconds: int(seconds), Size: size, BaseURL: info.ChannelBaseUrl, Proxy: info.ChannelSetting.Proxy}
	task.PrivateData.BillingContext = &model.TaskBillingContext{
		ModelPrice: pricePerSecond, GroupRatio: 1, OtherRatios: map[string]float64{"seconds": seconds},
		OriginModelName: modelName, PerCallBilling: true,
	}
	sum := sha256.Sum256(body)
	task, created, err := model.ReserveH3Task(task, fmt.Sprintf("%x", sum))
	if err != nil {
		status := http.StatusInternalServerError
		message := "Failed to reserve " + mediaType + " credit"
		if errors.Is(err, model.ErrH3IdempotencyConflict) {
			status, message = http.StatusConflict, "Idempotency-Key was already used with a different request"
		} else if errors.Is(err, model.ErrH3UnsafeBillingMode) {
			status, message = http.StatusServiceUnavailable, "Studio billing requires Redis and batch quota updates to be disabled"
		} else if errors.Is(err, model.ErrH3InsufficientQuota) {
			status, message = http.StatusPaymentRequired, "Insufficient wallet or token credit"
		}
		videoProxyError(c, status, "billing_error", message)
		return
	}
	if created {
		other := model.NewLogOther()
		other.MergePublic(map[string]any{"is_task": true, "task_id": task.TaskID, "model_price": pricePerSecond, "group_ratio": 1, "seconds": seconds, "billing_unit": "second", "currency": "USD"})
		logMessage := "H3 video: {{seconds}} seconds at ${{price}}/second"
		if isASR {
			logMessage = "Studio speech recognition: {{seconds}} seconds at ${{price}}/second"
		}
		model.RecordConsumeLog(c, task.UserId, model.RecordConsumeLogParams{
			ChannelId: task.ChannelId, ModelName: modelName, TokenName: c.GetString("token_name"),
			Quota: task.Quota, TokenId: info.TokenId, Group: task.Group,
			Content: []*common.Message{common.NewMessage(logMessage, map[string]any{"seconds": task.PrivateData.H3.Seconds, "price": strconv.FormatFloat(pricePerSecond, 'f', -1, 64)})},
			Other:   other,
		})
	}
	// Reconciliation failures leave the accepted task and frozen charge intact.
	// The existing persistent async task scheduler resumes it after a restart.
	if err := service.SyncH3Task(c.Request.Context(), task); err != nil {
		common.SysError(common.LogText("H3 task %s awaiting reconciliation: %v", task.TaskID, err))
	}
	writeH3Task(c, task, http.StatusAccepted)
}

func authorizeH3Task(c *gin.Context, task *model.Task) bool {
	if task.UserId != c.GetInt("id") || task.Platform != constant.TaskPlatformH3Studio {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return false
	}
	if common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		limits, ok := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
		allowed, valid := limits.(map[string]bool)
		if !ok || !valid || !allowed[ratio_setting.FormatMatchingModelName(task.Properties.OriginModelName)] {
			videoProxyError(c, http.StatusForbidden, "permission_error", "This token has no access to the task model")
			return false
		}
	}
	return true
}

func writeH3Task(c *gin.Context, task *model.Task, status int) {
	var body []byte
	var err error
	if task.Properties.OriginModelName == constant.StudioASRModel {
		body, err = h3studio.EncodeASRJob(task)
	} else {
		body, err = (&h3studio.TaskAdaptor{}).ConvertToOpenAIVideo(task)
	}
	if err != nil {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to encode Studio task")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(status, "application/json", body)
}

// RelayH3Inputs forwards only GPU upload receipts. The caller uploads media to
// the scoped GPU target returned by the video task, never through this gateway.
func RelayH3Inputs(c *gin.Context) {
	submitStudioInputs(c, false)
}

func RelayStudioASRInputs(c *gin.Context) {
	submitStudioInputs(c, true)
}

func submitStudioInputs(c *gin.Context, isASR bool) {
	id := c.Param("video_id")
	if id == "" {
		id = c.Param("task_id")
	}
	task, exists, err := model.GetByTaskId(c.GetInt("id"), id)
	if err != nil {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to query task")
		return
	}
	if !exists || task == nil || (task.Properties.OriginModelName == constant.StudioASRModel) != isASR {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return
	}
	if !authorizeH3Task(c, task) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024))
	var receipt map[string]any
	if err != nil || common.Unmarshal(body, &receipt) != nil || receipt == nil {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "A JSON upload receipt is required (maximum 1 MiB)")
		return
	}
	if isASR && len(receipt) != 0 {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "Speech recognition confirmation requires an empty JSON object")
		return
	}
	if isASR && (task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure) {
		writeH3Task(c, task, http.StatusOK)
		return
	}
	if err := service.SubmitH3Inputs(c.Request.Context(), task, body); err != nil {
		var inputErr *service.H3InputError
		if errors.As(err, &inputErr) {
			c.JSON(inputErr.Status, gin.H{"error": gin.H{"code": inputErr.Code, "message": "Refresh the task upload reservation and submit receipts for the current target"}})
			return
		}
		videoProxyError(c, http.StatusBadGateway, "upstream_error", "Upload receipt was not confirmed; query the task and retry against its current upload target")
		return
	}
	writeH3Task(c, task, http.StatusOK)
}

// relayH3TaskFetch keeps H3 reads on the same durable reconciliation path as
// submission instead of invoking the generic plugin polling/refund machinery.
func relayH3TaskFetch(c *gin.Context) bool {
	id := c.Param("task_id")
	if id == "" {
		return false
	}
	task, exists, err := model.GetByTaskId(c.GetInt("id"), id)
	if err != nil || !exists || task == nil || task.Platform != constant.TaskPlatformH3Studio {
		return false
	}
	if task.Properties.OriginModelName == constant.StudioASRModel {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return true
	}
	reconcileStudioTask(c, task)
	return true
}

func RelayStudioASRFetch(c *gin.Context) {
	task, exists, err := model.GetByTaskId(c.GetInt("id"), c.Param("task_id"))
	if err != nil {
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to query task")
		return
	}
	if !exists || task == nil || task.Properties.OriginModelName != constant.StudioASRModel {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return
	}
	reconcileStudioTask(c, task)
}

func reconcileStudioTask(c *gin.Context, task *model.Task) {
	if !authorizeH3Task(c, task) {
		return
	}
	if err := service.SyncH3Task(c.Request.Context(), task); err != nil {
		common.SysError(common.LogText("H3 task %s awaiting reconciliation: %v", task.TaskID, err))
	}
	writeH3Task(c, task, http.StatusOK)
}
