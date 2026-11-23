package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchModelsParsesDedupesSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q，期望 /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"b"},{"id":"a "},{"id":"b"},{"id":""}]}`)
	}))
	defer srv.Close()

	got, err := fetchModels(context.Background(), srv.Client(), "p", srv.URL+"/models",
		map[string]string{"Authorization": "Bearer k"})
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("模型列表 = %v，期望 [a b]", got)
	}
}

func TestFetchModelsEmptyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"  "}]}`)
	}))
	defer srv.Close()

	if _, err := fetchModels(context.Background(), srv.Client(), "p", srv.URL, nil); err == nil {
		t.Fatal("空列表应报错")
	}
}

func TestFetchModelsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchModels(context.Background(), srv.Client(), "p", srv.URL, nil)
	if err == nil {
		t.Fatal("非 2xx 应报错")
	}
	if !strings.Contains(err.Error(), "p") {
		t.Fatalf("错误应带供应商名：%v", err)
	}
}

func TestOpenAIModelsEndpointAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q，期望 /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-b"},{"id":"gpt-a"}]}`)
	}))
	defer srv.Close()

	a := newOpenAI("p", srv.URL+"/v1", "sk-test", "m", srv.Client())
	got, err := a.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 2 || got[0] != "gpt-a" || got[1] != "gpt-b" {
		t.Fatalf("模型列表 = %v", got)
	}
}

func TestAnthropicModelsAddsV1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q，期望 /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant" {
			t.Errorf("x-api-key = %q", got)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("缺少 anthropic-version")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-a"}]}`)
	}))
	defer srv.Close()

	a := newAnthropic("p", srv.URL, "sk-ant", "m", srv.Client())
	got, err := a.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 1 || got[0] != "claude-a" {
		t.Fatalf("模型列表 = %v", got)
	}
}
