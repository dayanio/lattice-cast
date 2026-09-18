package latticecast

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hostOf(srv *httptest.Server) string {
	u, err := url.Parse(srv.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func portOf(srv *httptest.Server) int {
	u, err := url.Parse(srv.URL)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

func TestPlay_SendsContractRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true,"state":"playing"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Play(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"},
		PlayRequest{URL: "http://192.168.1.10:7810/media/abc123", Title: "Interstellar"})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/play", gotPath)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	want, err := os.ReadFile("../../../../testdata/contract/play_request.json")
	require.NoError(t, err)
	var wantBody map[string]any
	require.NoError(t, json.Unmarshal(want, &wantBody))
	assert.Equal(t, wantBody, gotBody)
	assert.Equal(t, Status{State: "playing"}, st)
}

func TestPlay_AppError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"state":"error","error":"source_unreachable"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Play(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"},
		PlayRequest{URL: "http://192.168.1.10:7810/media/abc123"})
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "source_unreachable", err.Error())
	assert.Equal(t, "/play", pe.Op)
	assert.Equal(t, Status{}, st)
}

func TestPlay_HTTPUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	_, err := c.Play(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "wrong"},
		PlayRequest{URL: "http://192.168.1.10:7810/media/abc123"})
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "unauthorized", err.Error())
}

func TestPause(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true,"state":"paused"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Pause(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/pause", gotPath)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	assert.Empty(t, gotBody) // /pause 不携带请求体
	assert.Equal(t, Status{State: "paused"}, st)
}

func TestStop(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true,"state":"idle"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Stop(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/stop", gotPath)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	assert.Empty(t, gotBody) // /stop 不携带请求体
	assert.Equal(t, Status{State: "idle"}, st)
}

func TestSeek_SendsBody(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true,"state":"playing"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Seek(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"},
		SeekRequest{PositionMS: 90000})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/seek", gotPath)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	assert.Equal(t, map[string]any{"position_ms": float64(90000)}, gotBody)
	assert.Equal(t, Status{State: "playing"}, st) // seek 不改变播放状态
}

func TestVolume_SendsContractRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true,"state":"playing"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Volume(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"},
		VolumeRequest{Level: 42})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/volume", gotPath)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	want, err := os.ReadFile("../../../../testdata/contract/volume_request.json")
	require.NoError(t, err)
	var wantBody map[string]any
	require.NoError(t, json.Unmarshal(want, &wantBody))
	assert.Equal(t, wantBody, gotBody)
	assert.Equal(t, Status{State: "playing"}, st)
}

func TestStatus_MapsFullResponse(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	fixture, err := os.ReadFile("../../../../testdata/contract/status_response.json")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.Write(fixture) // 200 响应不含 ok 字段，以 state 为准
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	st, err := c.Status(context.Background(), Target{Host: hostOf(srv), Port: portOf(srv), Token: "s3cret"})
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/status", gotPath)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	assert.Equal(t, Status{
		State:      "playing",
		PositionMS: 42000,
		DurationMS: 5400000,
		Title:      "Interstellar",
		Error:      "",
	}, st)
}

func TestStatus_Offline(t *testing.T) {
	// 占住一个端口后立即关闭：向它发请求必然连接拒绝（渲染端离线的形态）。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host := l.Addr().(*net.TCPAddr).IP.String()
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	c := NewClient(&http.Client{})
	_, err = c.Status(context.Background(), Target{Host: host, Port: port, Token: "s3cret"})
	require.Error(t, err)
	var ue *url.Error
	require.ErrorAs(t, err, &ue) // Manager 依据 *url.Error 判定渲染端离线
}
