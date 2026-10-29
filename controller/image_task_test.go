package controller

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageAsyncRequested(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, "/v1/images/edits?async=true", nil)
		require.NoError(t, err)
		assert.True(t, imageAsyncRequested(request, nil))
	})

	t.Run("json", func(t *testing.T) {
		body := []byte(`{"model":"gpt-image-2","async":true}`)
		request, err := http.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		assert.True(t, imageAsyncRequested(request, body))
	})

	t.Run("multipart", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("model", "gpt-image-2"))
		require.NoError(t, writer.WriteField("async", "true"))
		require.NoError(t, writer.Close())

		request, err := http.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
		require.NoError(t, err)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		assert.True(t, imageAsyncRequested(request, body.Bytes()))
	})

	t.Run("default synchronous", func(t *testing.T) {
		body := []byte(`{"model":"gpt-image-2"}`)
		request, err := http.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		assert.False(t, imageAsyncRequested(request, body))
	})
}
