package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

type imageTaskRequestData struct {
	Path string          `json:"path"`
	Data []dto.ImageData `json:"data,omitempty"`
}

type imageTaskResponse struct {
	ID        string             `json:"id"`
	Object    string             `json:"object"`
	Status    string             `json:"status"`
	CreatedAt int64              `json:"created_at"`
	UpdatedAt int64              `json:"updated_at,omitempty"`
	URL       string             `json:"url,omitempty"`
	Data      []dto.ImageData    `json:"data,omitempty"`
	Error     *types.OpenAIError `json:"error,omitempty"`
}

// RelayImage keeps the OpenAI-compatible synchronous behavior by default and
// creates a background task when async=true is supplied by query, header, JSON,
// or multipart form field.
func RelayImage(c *gin.Context) {
	bodyStorage, err := common.GetBodyStorage(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.OpenAIError{Message: err.Error(), Type: "invalid_request_error"}})
		return
	}
	body, err := bodyStorage.Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.OpenAIError{Message: err.Error(), Type: "invalid_request_error"}})
		return
	}

	if !imageAsyncRequested(c.Request, body) {
		c.Request.Body = http.NoBody
		if len(body) > 0 {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		Relay(c, types.RelayFormatOpenAIImage)
		return
	}

	now := time.Now().Unix()
	task := &model.Task{
		CreatedAt:  now,
		UpdatedAt:  now,
		TaskID:     model.GenerateTaskID(),
		Platform:   constant.TaskPlatformImage,
		UserId:     c.GetInt("id"),
		Group:      common.GetContextKeyString(c, constant.ContextKeyUsingGroup),
		ChannelId:  common.GetContextKeyInt(c, constant.ContextKeyChannelId),
		Action:     strings.TrimPrefix(c.Request.URL.Path, "/v1/images/"),
		Status:     model.TaskStatusQueued,
		SubmitTime: now,
		Progress:   "0%",
	}
	task.SetData(imageTaskRequestData{Path: c.Request.URL.Path})
	if err := task.Insert(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": types.OpenAIError{Message: "failed to create image task", Type: "server_error"}})
		return
	}

	request := c.Request.Clone(context.Background())
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.MultipartForm = nil
	request.Header = c.Request.Header.Clone()
	request.Header.Set("X-New-API-Image-Task", task.TaskID)
	contextCopy := c.Copy()
	keys := make(map[string]any, len(contextCopy.Keys))
	for key, value := range contextCopy.Keys {
		keys[key] = value
	}
	delete(keys, common.KeyBodyStorage)
	delete(keys, common.KeyRequestBody)

	gopool.Go(func() {
		runImageTask(task, request, keys)
	})

	c.JSON(http.StatusAccepted, imageTaskResponse{
		ID: task.TaskID, Object: "image.task", Status: "queued", CreatedAt: task.CreatedAt,
	})
}

func ImageTaskFetch(c *gin.Context) {
	task, exists, err := model.GetByTaskId(c.GetInt("id"), c.Param("task_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": types.OpenAIError{Message: "failed to query image task", Type: "server_error"}})
		return
	}
	if !exists || task.Platform != constant.TaskPlatformImage {
		c.JSON(http.StatusNotFound, gin.H{"error": types.OpenAIError{Message: "image task not found", Type: "invalid_request_error"}})
		return
	}

	response := imageTaskResponse{
		ID: task.TaskID, Object: "image.task", Status: imageTaskStatus(task.Status),
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
	if task.Status == model.TaskStatusSuccess {
		response.URL = task.PrivateData.ResultURL
		var result imageTaskRequestData
		if common.Unmarshal(task.Data, &result) == nil {
			response.Data = result.Data
		}
	}
	if task.Status == model.TaskStatusFailure {
		response.Error = &types.OpenAIError{Message: task.FailReason, Type: "image_generation_error"}
	}
	c.JSON(http.StatusOK, response)
}

func runImageTask(task *model.Task, request *http.Request, keys map[string]any) {
	oldStatus := task.Status
	task.Status = model.TaskStatusInProgress
	task.StartTime = time.Now().Unix()
	task.Progress = "50%"
	won, err := task.UpdateWithStatus(oldStatus)
	if err != nil || !won {
		return
	}

	recorder := httptest.NewRecorder()
	workerContext, _ := gin.CreateTestContext(recorder)
	workerContext.Request = request
	workerContext.Keys = keys
	Relay(workerContext, types.RelayFormatOpenAIImage)

	task.FinishTime = time.Now().Unix()
	task.Progress = "100%"
	if recorder.Code < http.StatusOK || recorder.Code >= http.StatusMultipleChoices {
		task.Status = model.TaskStatusFailure
		task.FailReason = strings.TrimSpace(recorder.Body.String())
		if task.FailReason == "" {
			task.FailReason = fmt.Sprintf("image request failed with status %d", recorder.Code)
		}
	} else {
		var response dto.ImageResponse
		if err := common.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Data) == 0 {
			task.Status = model.TaskStatusFailure
			task.FailReason = "image provider returned an invalid response"
		} else if response.Data[0].Url == "" && response.Data[0].B64Json == "" {
			task.Status = model.TaskStatusFailure
			task.FailReason = "image provider did not return image data"
		} else {
			task.Status = model.TaskStatusSuccess
			task.PrivateData.ResultURL = response.Data[0].Url
			task.SetData(imageTaskRequestData{Path: request.URL.Path, Data: response.Data})
		}
	}
	if err := task.Update(); err != nil {
		logger.LogError(context.Background(), fmt.Sprintf("update image task %s failed: %v", task.TaskID, err))
	}
}

func imageAsyncRequested(request *http.Request, body []byte) bool {
	if strings.EqualFold(request.URL.Query().Get("async"), "true") || strings.EqualFold(request.Header.Get("X-Async"), "true") {
		return true
	}
	mediaType, params, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				return false
			}
			if part.FormName() == "async" {
				value := make([]byte, 5)
				n, _ := part.Read(value)
				return strings.EqualFold(strings.TrimSpace(string(value[:n])), "true")
			}
		}
	}
	if mediaType == "application/json" {
		var payload map[string]any
		if common.Unmarshal(body, &payload) == nil {
			value, _ := payload["async"].(bool)
			return value
		}
	}
	return false
}

func imageTaskStatus(status model.TaskStatus) string {
	switch status {
	case model.TaskStatusQueued, model.TaskStatusNotStart:
		return "queued"
	case model.TaskStatusInProgress, model.TaskStatusSubmitted:
		return "processing"
	case model.TaskStatusSuccess:
		return "completed"
	case model.TaskStatusFailure:
		return "failed"
	default:
		return "unknown"
	}
}
