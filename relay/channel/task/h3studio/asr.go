package h3studio

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

var asrClipID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type asrClip struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	DurationMs  int64  `json:"durationMs"`
}

type asrRequest struct {
	RequestKey string    `json:"requestKey"`
	Language   string    `json:"language"`
	Clips      []asrClip `json:"clips"`
}

type asrJobError struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	ProviderTerminal bool   `json:"providerTerminal,omitempty"`
}

type asrJob struct {
	ID       string `json:"id,omitempty"`
	Model    string `json:"model,omitempty"`
	Status   string `json:"status"`
	Stage    string `json:"stage"`
	Progress *struct {
		Percent *float64 `json:"percent,omitempty"`
	} `json:"progress,omitempty"`
	WorkerTiming *struct {
		QueuedAt   int64 `json:"queuedAt,omitempty"`
		StartedAt  int64 `json:"startedAt,omitempty"`
		FinishedAt int64 `json:"finishedAt,omitempty"`
	} `json:"workerTiming,omitempty"`
	Billing *struct {
		Unit             string `json:"unit"`
		RequestedSeconds int    `json:"requestedSeconds"`
	} `json:"billing,omitempty"`
	Upload *struct {
		Token string `json:"token"`
		Files []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"files"`
	} `json:"upload,omitempty"`
	Result *struct {
		Transcripts []struct {
			ID         string `json:"id"`
			Text       string `json:"text"`
			DurationMs int64  `json:"durationMs,omitempty"`
		} `json:"transcripts"`
	} `json:"result,omitempty"`
	Error *asrJobError `json:"error,omitempty"`
}

func BuildASRRequest(c *gin.Context, requestKey string) ([]byte, float64, error) {
	var input struct {
		Model    string    `json:"model"`
		Language string    `json:"language"`
		Clips    []asrClip `json:"clips"`
	}
	if c.ContentType() != "application/json" || common.UnmarshalBodyReusable(c, &input) != nil {
		return nil, 0, fmt.Errorf("speech recognition requires a JSON clip manifest")
	}
	if input.Model != constant.StudioASRModel || len(input.Clips) == 0 || len(input.Clips) > 128 {
		return nil, 0, fmt.Errorf("speech recognition requires model %s and 1 to 128 audio clips", constant.StudioASRModel)
	}
	if input.Language == "" {
		input.Language = "Chinese"
	}
	if input.Language != "Chinese" && input.Language != "Auto" {
		return nil, 0, fmt.Errorf("speech recognition language must be Chinese or Auto")
	}
	knownIDs := make(map[string]bool, len(input.Clips))
	const maxSafeMillis = 1<<53 - 1
	var durationMs int64
	for _, clip := range input.Clips {
		if !asrClipID.MatchString(clip.ID) || knownIDs[clip.ID] || !mediaHash.MatchString(clip.SHA256) ||
			len(clip.Filename) == 0 || len(clip.Filename) > 255 || strings.ContainsAny(clip.Filename, "/\\\x00") ||
			(!strings.HasPrefix(clip.ContentType, "audio/") && clip.ContentType != "application/octet-stream") ||
			clip.Size < 1 || clip.Size > 32<<20 || clip.DurationMs <= 0 || clip.DurationMs > maxSafeMillis-durationMs {
			return nil, 0, fmt.Errorf("invalid speech recognition clip descriptor")
		}
		knownIDs[clip.ID] = true
		durationMs += clip.DurationMs
	}
	body, err := common.Marshal(asrRequest{RequestKey: requestKey, Language: input.Language, Clips: input.Clips})
	return body, float64((durationMs + 999) / 1000), err
}

func decodeASRJob(body []byte) (*asrJob, error) {
	var job asrJob
	if common.Unmarshal(body, &job) != nil || job.ID == "" || job.Status == "" || job.Stage == "" {
		return nil, fmt.Errorf("invalid Studio speech recognition response")
	}
	if job.Stage != "awaiting_upload" {
		job.Upload = nil
	}
	if job.Status != "succeeded" {
		job.Result = nil
	}
	if job.Error != nil {
		// The service's native messages can include an instance or storage path.
		if !asrClipID.MatchString(job.Error.Code) {
			job.Error.Code = "ASR_FAILED"
		}
		job.Error.Message = "Speech recognition failed"
		if job.Error.Code == "ASR_INPUTS_MISSING" {
			job.Error.Message = "Upload the remaining audio clips and confirm the task again"
		}
	}
	return &job, nil
}

func parseASRResult(task *model.Task, body []byte) (*relaycommon.TaskInfo, error) {
	job, err := decodeASRJob(body)
	if err != nil {
		return nil, err
	}
	result := &relaycommon.TaskInfo{TaskID: job.ID, Status: model.TaskStatusInProgress, Progress: "30%"}
	switch job.Status {
	case "queued":
		result.Status, result.Progress = model.TaskStatusQueued, "10%"
	case "running":
		if job.Stage == "awaiting_upload" {
			if job.Upload == nil || job.Upload.Token == "" || len(job.Upload.Files) == 0 {
				return nil, fmt.Errorf("speech recognition upload reservation is missing")
			}
			for _, file := range job.Upload.Files {
				target, err := url.Parse(file.URL)
				if err != nil || target.Host == "" || target.User != nil || (target.Scheme != "https" && target.Scheme != "http") {
					return nil, fmt.Errorf("invalid speech recognition upload target")
				}
			}
		}
	case "succeeded":
		var request asrRequest
		if job.Result == nil || task.PrivateData.H3 == nil ||
			common.Unmarshal(task.PrivateData.H3.RequestBody, &request) != nil ||
			len(request.Clips) == 0 || len(job.Result.Transcripts) != len(request.Clips) {
			return nil, fmt.Errorf("speech recognition result does not match the frozen clip manifest")
		}
		expected := make(map[string]bool, len(request.Clips))
		for _, clip := range request.Clips {
			expected[clip.ID] = true
		}
		for _, transcript := range job.Result.Transcripts {
			if !expected[transcript.ID] {
				return nil, fmt.Errorf("speech recognition result does not match the frozen clip manifest")
			}
			delete(expected, transcript.ID)
		}
		result.Status, result.Progress = model.TaskStatusSuccess, "100%"
	case "failed", "cancelled":
		if job.Error != nil && job.Error.ProviderTerminal {
			result.Status, result.Progress, result.Reason = model.TaskStatusFailure, "100%", job.Error.Code
		}
	default:
		return nil, fmt.Errorf("invalid speech recognition task status")
	}
	return result, nil
}

func EncodeASRJob(task *model.Task) ([]byte, error) {
	var job asrJob
	if len(task.Data) > 0 && common.Unmarshal(task.Data, &job) != nil {
		return nil, fmt.Errorf("invalid stored speech recognition result")
	}
	job.ID = task.TaskID
	job.Model = constant.StudioASRModel
	if job.Stage == "" {
		job.Status, job.Stage = "queued", "queued"
	}
	if task.Status != model.TaskStatusFailure && (job.Status == "failed" || job.Status == "cancelled") {
		job.Status, job.Stage = "running", "running"
	}
	if task.Status == model.TaskStatusFailure {
		job.Upload, job.Result = nil, nil
		job.Status, job.Stage = "failed", "failed"
		job.Error = &asrJobError{Code: task.FailReason, Message: "Speech recognition failed", ProviderTerminal: true}
	}
	if task.Status == model.TaskStatusSuccess {
		job.Upload = nil
	}
	return common.Marshal(job)
}

func (a *TaskAdaptor) SanitizeASRTaskData(body []byte) ([]byte, error) {
	job, err := decodeASRJob(body)
	if err != nil {
		return nil, err
	}
	job.ID, job.Model = "", ""
	return common.Marshal(job)
}
