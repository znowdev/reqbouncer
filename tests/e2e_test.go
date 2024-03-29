package tests

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/znowdev/reqbouncer/internal/client"
	"github.com/znowdev/reqbouncer/internal/client/auth"
	"github.com/znowdev/reqbouncer/internal/server"
	"github.com/znowdev/reqbouncer/internal/slogger"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestE2E(t *testing.T) {
	slogger.NewSlogger(true)
	targetPort := "50000"
	serverPort := "50001"
	logger, _ := slogger.NewSlogger(true)
	go func() {
		// Start server
		err := server.Start(logger, server.Config{
			GithubUserProvider: func(token string) (auth.GitHubUser, error) {
				return auth.GitHubUser{
					Login: "client1",
				}, nil
			},
			Port: serverPort,
		})
		if err != nil {
			t.Fatalf("failed to start server: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	// Start client

	go func() {
		// Start client
		client, err := client.NewClient(client.Config{
			Target:      "localhost:" + targetPort,
			Server:      "localhost:" + serverPort,
			Path:        "/_websocket",
			AccessToken: "secret",
		})
		if err != nil {
			t.Fatalf("failed to start client: %v", err)
		}
		err = client.Listen(context.Background())
		if err != nil {
			t.Fatalf("failed to listen: %v", err)

		}
	}()

	go func() {
		// Start target
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			slog.Debug("received request in target")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Hello, world!"))
		})
		mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
			slog.Debug("received echo request in target")
			w.WriteHeader(http.StatusOK)
			io.Copy(w, r.Body)
		})
		err := http.ListenAndServe(":"+targetPort, mux)
		if err != nil {
			t.Fatalf("failed to start target: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// E2ETest
	t.Run("Server health check", func(t *testing.T) {
		resp, err := http.Get("http://localhost:" + serverPort + "/_health")
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status code %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})

	t.Run("Target GET", func(t *testing.T) {
		resp, err := http.Get("http://localhost:" + serverPort + "/")
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status code %d, got %d", http.StatusOK, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}

		if string(body) != "Hello, world!" {
			t.Fatalf("expected body %s, got %s", "Hello, world!", body)
		}
	})

	t.Run("Target POST", func(t *testing.T) {
		resp, err := http.Post("http://localhost:"+serverPort+"/echo", "text/plain", strings.NewReader("Hello, world!"))
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)

			t.Fatalf("expected status code %d, got %d (%s)", http.StatusOK, resp.StatusCode, body)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}

		if string(body) != "Hello, world!" {
			t.Fatalf("expected body %s, got %s", "Hello, world!", body)
		}
	})

	t.Run("Target GET with client id", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://localhost:"+serverPort+"/", nil)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		req.Header.Set("reqbouncer-client-id", "client1")
		resp, err := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status code %d, got %d (%s)", http.StatusOK, resp.StatusCode, body)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}

		if string(body) != "Hello, world!" {
			t.Fatalf("expected body %s, got %s", "Hello, world!", body)
		}
	})

}

func TestE2EMultiClientsWithSameId(t *testing.T) {
	logger, _ := slogger.NewSlogger(true)

	targetPort := "50002"
	serverPort := "50003"

	go func() {
		// Start server
		err := server.Start(logger, server.Config{
			GithubUserProvider: func(token string) (auth.GitHubUser, error) {
				return auth.GitHubUser{
					Login: "client1",
				}, nil
			},
			Port: serverPort,
		})
		if err != nil {
			t.Fatalf("failed to start server: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	// Start client

	go func() {
		// Start client
		client, err := client.NewClient(client.Config{
			Target:      "localhost:" + targetPort,
			Server:      "localhost:" + serverPort,
			Path:        "/_websocket",
			AccessToken: "secret",
		})
		if err != nil {
			t.Fatalf("failed to start client: %v", err)
		}
		err = client.Listen(context.Background())
		if err != nil {
			t.Fatalf("failed to listen: %v", err)

		}
	}()

	time.Sleep(100 * time.Millisecond)

	client, err := client.NewClient(client.Config{
		Target:      "localhost:" + targetPort,
		Server:      "localhost:" + serverPort,
		Path:        "/_websocket",
		AccessToken: "secret",
	})
	if err != nil {
		t.Fatalf("failed to start client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	doneCh := make(chan struct{})
	var gotErr error
	defer cancel()
	go func() {
		defer close(doneCh)
		err := client.Listen(context.Background())
		if err != nil {
			gotErr = err
		}

		doneCh <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		break
	case <-doneCh:
		break
	}
	if gotErr == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestE2EServerUtilEndpoints(t *testing.T) {
	logger, _ := slogger.NewSlogger(true)
	serverPort := "50005"

	go func() {
		// Start server
		err := server.Start(logger, server.Config{
			GithubClientid: "client1",
			GithubUserProvider: func(token string) (auth.GitHubUser, error) {
				return auth.GitHubUser{
					Login: "client1",
				}, nil
			},
			Port: serverPort,
		})
		if err != nil {
			t.Fatalf("failed to start server: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	// Start client

	t.Run("health", func(t *testing.T) {
		resp, err := http.Get("http://localhost:" + serverPort + "/_health")
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status code %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})

	t.Run("config", func(t *testing.T) {
		resp, err := http.Get("http://localhost:" + serverPort + "/_config")
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status code %d, got %d", http.StatusOK, resp.StatusCode)
		}
		var m map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&m)
		require.NoError(t, err)
		require.Equal(t, m["github_client_id"], "client1")

		githubClientId, err := auth.GetGithubConfig("http://localhost:" + serverPort)
		require.NoError(t, err)
		require.Equal(t, "client1", githubClientId)
	})
}
