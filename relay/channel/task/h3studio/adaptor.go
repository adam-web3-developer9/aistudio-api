package h3studio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	ModelName      = "h3"
	WorkflowID     = "minimax-h3-9plus3"
	PricePerSecond = constant.H3PricePerSecond
	requestContext = "h3_studio_request"
)

var _ channel.TaskAdaptor = (*TaskAdaptor)(nil)

var (
	mediaBinding  = regexp.MustCompile(`^(image[1-9]|audio[1-3])$`)
	mediaFilename = regexp.MustCompile(`(?i)^[A-Za-z0-9_-]+\.(png|jpg|jpeg|webp|wav|mp3)$`)
	mediaHash     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type DirectMedia struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
}

// StudioRequest contains only metadata. Media bytes travel directly to the GPU.
type StudioRequest struct {
	AdapterID      string                 `json:"adapterId"`
	RequestKey     string                 `json:"requestKey"`
	WorkflowID     string                 `json:"workflowId"`
	Values         map[string]any         `json:"values"`
	Media          map[string]string      `json:"media"`
	Transport      string                 `json:"transport"`
	DirectMedia    map[string]DirectMedia `json:"directMedia"`
	ResultDelivery string                 `json:"resultDelivery"`
}

type StudioJob struct {
	ID      string `json:"id,omitempty"`
	Status  string `json:"status"`
	Stage   string `json:"stage"`
	Billing *struct {
		Unit             string `json:"unit"`
		RequestedSeconds int    `json:"requestedSeconds"`
	} `json:"billing,omitempty"`
	Upload *struct {
		BaseURL       string `json:"baseUrl"`
		Subfolder     string `json:"subfolder"`
		ReservationID string `json:"reservationId"`
		Files         []struct {
			Binding string `json:"binding"`
			DirectMedia
		} `json:"files"`
	} `json:"upload,omitempty"`
	Result *struct {
		VideoURL  string `json:"videoUrl"`
		Storage   string `json:"storage"`
		ExpiresAt int64  `json:"expiresAt"`
	} `json:"result,omitempty"`
	Error *struct {
		Code             string `json:"code"`
		Message          string `json:"message"`
		ProviderTerminal *bool  `json:"providerTerminal,omitempty"`
	} `json:"error,omitempty"`
}

type TaskAdaptor struct {
	taskcommon.BaseBilling
	baseURL string
	apiKey  string
}

// SubmissionRejectedError is only emitted after an idempotency lookup proved
// the task absent and the subsequent create was explicitly rejected.
type SubmissionRejectedError struct {
	StatusCode int
}

func (e *SubmissionRejectedError) Error() string {
	return fmt.Sprintf("H3 submission rejected with HTTP %d", e.StatusCode)
}

func (e *SubmissionRejectedError) DefinitiveRejection() bool { return true }

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.baseURL = strings.TrimRight(info.ChannelBaseUrl, "/")
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *taskdto.TaskError {
	var req struct {
		Model          string                     `json:"model"`
		Prompt         string                     `json:"prompt"`
		Seconds        json.RawMessage            `json:"seconds"`
		Duration       json.RawMessage            `json:"duration"`
		Size           string                     `json:"size"`
		Image          string                     `json:"image"`
		Images         []string                   `json:"images"`
		InputReference string                     `json:"input_reference"`
		Metadata       map[string]json.RawMessage `json:"metadata"`
	}
	invalid := func(err error) *taskdto.TaskError {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
		return invalid(fmt.Errorf("H3 accepts JSON metadata; upload media directly using metadata.direct_media"))
	}
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return invalid(err)
	}
	if req.Model != ModelName {
		return invalid(fmt.Errorf("unsupported H3 model"))
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return invalid(fmt.Errorf("prompt is required"))
	}
	if req.Image != "" || len(req.Images) > 0 || req.InputReference != "" {
		return invalid(fmt.Errorf("H3 reference media requires metadata.direct_media and the direct upload handshake"))
	}
	seconds := 5
	specifiedSeconds := 0
	for _, raw := range []json.RawMessage{req.Duration, req.Seconds} {
		if len(raw) == 0 {
			continue
		}
		var value int
		if err := common.Unmarshal(raw, &value); err != nil || string(raw) == "null" {
			var text string
			if err := common.Unmarshal(raw, &text); err != nil {
				return invalid(fmt.Errorf("seconds and duration must be integers"))
			}
			var parseErr error
			value, parseErr = strconv.Atoi(text)
			if parseErr != nil {
				return invalid(fmt.Errorf("seconds and duration must be integers"))
			}
		}
		if value < 4 || value > 15 || value > relaycommon.MaxTaskDurationSeconds {
			return invalid(fmt.Errorf("H3 duration must be between 4 and 15 seconds"))
		}
		if specifiedSeconds != 0 && specifiedSeconds != value {
			return invalid(fmt.Errorf("seconds and duration must match"))
		}
		specifiedSeconds = value
		seconds = value
	}
	width, height := 1344, 768
	if req.Size != "" {
		parts := strings.Split(req.Size, "x")
		if len(parts) != 2 {
			return invalid(fmt.Errorf("size must be WIDTHxHEIGHT"))
		}
		var err error
		width, err = strconv.Atoi(parts[0])
		if err != nil {
			return invalid(fmt.Errorf("size must be WIDTHxHEIGHT"))
		}
		height, err = strconv.Atoi(parts[1])
		if err != nil {
			return invalid(fmt.Errorf("size must be WIDTHxHEIGHT"))
		}
	}
	if width < 480 || width > 1920 || height < 480 || height > 1920 || width%32 != 0 || height%32 != 0 || width*height > 1920*1088 {
		return invalid(fmt.Errorf("H3 dimensions must be multiples of 32 between 480 and 1920, with at most 2088960 pixels"))
	}
	directMedia := map[string]DirectMedia{}
	for name, raw := range req.Metadata {
		if name != "direct_media" {
			return invalid(fmt.Errorf("unsupported H3 metadata field: %s", name))
		}
		if err := common.Unmarshal(raw, &directMedia); err != nil || directMedia == nil {
			return invalid(fmt.Errorf("direct_media must be an object"))
		}
	}
	for binding, media := range directMedia {
		if !mediaBinding.MatchString(binding) || len(media.Filename) > 200 || !mediaFilename.MatchString(media.Filename) || !mediaHash.MatchString(media.SHA256) {
			return invalid(fmt.Errorf("invalid direct media descriptor: %s", binding))
		}
		contentTypes := map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "webp": "image/webp", "wav": "audio/wav", "mp3": "audio/mpeg"}
		extension := strings.ToLower(media.Filename[strings.LastIndex(media.Filename, ".")+1:])
		if contentTypes[extension] != media.ContentType || strings.HasPrefix(binding, "image") != strings.HasPrefix(media.ContentType, "image/") {
			return invalid(fmt.Errorf("direct media type does not match binding or filename: %s", binding))
		}
	}
	info.Action = constant.TaskActionTextToVideo
	if len(directMedia) > 0 {
		info.Action = constant.TaskActionReferenceToVideo
	}
	c.Set("task_request", relaycommon.TaskSubmitReq{Model: ModelName, Prompt: req.Prompt, Duration: seconds, Seconds: strconv.Itoa(seconds), Size: fmt.Sprintf("%dx%d", width, height)})
	c.Set(requestContext, StudioRequest{AdapterID: "comfyui-v1", WorkflowID: WorkflowID, Values: map[string]any{"prompt": req.Prompt, "duration": seconds, "width": width, "height": height}, Media: map[string]string{}, Transport: "direct", DirectMedia: directMedia, ResultDelivery: "archive"})
	return nil
}

func (a *TaskAdaptor) EstimateBilling(c *gin.Context, _ *relaycommon.RelayInfo) map[string]float64 {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil
	}
	return map[string]float64{"seconds": float64(req.Duration)}
}

func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return a.baseURL + "/v1/jobs", nil
}

func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	v, exists := c.Get(requestContext)
	if !exists || info.PublicTaskID == "" {
		return nil, fmt.Errorf("validated H3 request and persisted public task ID are required")
	}
	req, ok := v.(StudioRequest)
	if !ok {
		return nil, fmt.Errorf("invalid H3 request")
	}
	req.RequestKey = info.PublicTaskID
	body, err := common.Marshal(req)
	return bytes.NewReader(body), err
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, body)
}

// ParseResponse parses only the short acknowledgement. Durable H3 submissions
// are reconciled through FetchTask using the original persisted request key.
func (a *TaskAdaptor) ParseResponse(_ *gin.Context, resp *http.Response, _ *relaycommon.RelayInfo) (*channel.TaskSubmitResponse, *taskdto.TaskError) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusBadGateway)
	}
	var acknowledgement struct {
		JobID string `json:"jobId"`
	}
	if err := common.Unmarshal(body, &acknowledgement); err != nil || acknowledgement.JobID == "" {
		return nil, service.TaskErrorWrapper(fmt.Errorf("invalid H3 submit response"), "invalid_response", http.StatusBadGateway)
	}
	return &channel.TaskSubmitResponse{UpstreamTaskID: acknowledgement.JobID}, nil
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	if task == nil {
		return nil, fmt.Errorf("H3 task is required")
	}
	id := task.TaskID
	if id == "" {
		return nil, fmt.Errorf("H3 task_id is required")
	}
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, err
	}
	// Bound each metadata request even when the global relay timeout is disabled.
	boundedClient := *client
	boundedClient.Timeout = 30 * time.Second
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/jobs/" + url.PathEscape(id)
	if strings.HasPrefix(id, "task_") {
		endpoint = strings.TrimRight(baseURL, "/") + "/v1/jobs/by-request-key/" + url.PathEscape(id)
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	resp, err := boundedClient.Do(req)
	if err != nil {
		return nil, err
	}
	creating := false
	if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(id, "task_") {
		_ = resp.Body.Close()
		if task.PrivateData.H3 == nil || len(task.PrivateData.H3.RequestBody) == 0 {
			return nil, fmt.Errorf("persisted H3 request body is required for recovery")
		}
		req, err = http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/jobs", bytes.NewReader(task.PrivateData.H3.RequestBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("Content-Type", "application/json")
		creating = true
		resp, err = boundedClient.Do(req)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		_ = resp.Body.Close()
		if creating && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnprocessableEntity) {
			return nil, &SubmissionRejectedError{StatusCode: resp.StatusCode}
		}
		return nil, fmt.Errorf("H3 upstream returned HTTP %d", resp.StatusCode)
	}
	if creating {
		// Studio's create response is only an acknowledgement (jobId/statusUrl),
		// not the full job projection needed for billing checks and upload targets.
		// Query the same persisted key after acceptance, including on idempotent replay.
		_ = resp.Body.Close()
		req, err = http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", key)
		resp, err = boundedClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("H3 accepted task lookup returned HTTP %d", resp.StatusCode)
		}
	}
	return resp, nil
}

func (a *TaskAdaptor) ParseTaskResult(_ *model.Task, _ *http.Response, body []byte) (*relaycommon.TaskInfo, error) {
	var job StudioJob
	if err := common.Unmarshal(body, &job); err != nil || job.ID == "" || job.Status == "" {
		return nil, fmt.Errorf("invalid H3 task response")
	}
	result := &relaycommon.TaskInfo{TaskID: job.ID, Status: model.TaskStatusInProgress, Progress: "30%"}
	if job.Status == "queued" {
		result.Status = model.TaskStatusQueued
		result.Progress = "10%"
	}
	if job.Status == "succeeded" && job.Result != nil && job.Result.Storage == "tos" && job.Result.VideoURL != "" {
		if job.Billing == nil || job.Billing.Unit != "second" || job.Billing.RequestedSeconds < 4 || job.Billing.RequestedSeconds > 15 {
			return nil, fmt.Errorf("H3 completed task has no valid frozen billing duration")
		}
		parsed, err := url.Parse(job.Result.VideoURL)
		if err != nil || parsed.Host == "" || parsed.Scheme != "https" && parsed.Scheme != "http" {
			return nil, fmt.Errorf("invalid H3 archived result URL")
		}
		result.Status, result.Progress, result.Url = model.TaskStatusSuccess, "100%", job.Result.VideoURL
	}
	if (job.Status == "failed" || job.Status == "cancelled") && job.Error != nil && job.Error.ProviderTerminal != nil && *job.Error.ProviderTerminal {
		result.Status, result.Progress, result.Reason = model.TaskStatusFailure, "100%", job.Error.Code
	}
	return result, nil
}

// SanitizeTaskData excludes operational events, credentials, instance details,
// and native identifiers from persisted public task data.
func (a *TaskAdaptor) SanitizeTaskData(body []byte) ([]byte, error) {
	var job StudioJob
	if err := common.Unmarshal(body, &job); err != nil || job.ID == "" || job.Status == "" {
		return nil, fmt.Errorf("invalid H3 task response")
	}
	job.ID = ""
	if job.Error != nil {
		job.Error.Message = ""
	}
	return common.Marshal(job)
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(task *model.Task) ([]byte, error) {
	var job StudioJob
	if len(task.Data) > 0 {
		if err := common.Unmarshal(task.Data, &job); err != nil {
			return nil, fmt.Errorf("invalid stored H3 task response: %w", err)
		}
	}
	video := task.ToOpenAIVideo()
	if task.Status != model.TaskStatusFailure && task.Status != model.TaskStatusSuccess {
		video.CompletedAt = 0
	}
	video.SetMetadata("stage", job.Stage)
	if task.PrivateData.BillingContext != nil {
		seconds := task.PrivateData.BillingContext.OtherRatios["seconds"]
		if task.PrivateData.H3 != nil {
			seconds = float64(task.PrivateData.H3.Seconds)
			video.Size = task.PrivateData.H3.Size
		}
		video.Seconds = strconv.FormatFloat(seconds, 'f', -1, 64)
		video.SetMetadata("billing", map[string]any{"unit": "second", "requested_seconds": seconds, "price_per_second": task.PrivateData.BillingContext.ModelPrice, "currency": "USD"})
	}
	if job.Upload != nil && task.Status != model.TaskStatusFailure && task.Status != model.TaskStatusSuccess {
		video.SetMetadata("upload", job.Upload)
	}
	if job.Result != nil && task.Status == model.TaskStatusSuccess {
		video.ExpiresAt = job.Result.ExpiresAt / 1000
	}
	if task.Status == model.TaskStatusFailure {
		video.Error = &dto.OpenAIVideoError{Code: task.FailReason, Message: "H3 generation failed"}
	}
	return common.Marshal(video)
}

func (a *TaskAdaptor) GetModelList() []string { return []string{ModelName} }
func (a *TaskAdaptor) GetChannelName() string { return "H3 Studio" }
