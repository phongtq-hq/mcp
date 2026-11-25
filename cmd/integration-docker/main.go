// Command integration-docker: managed MCP server chỉ đọc cho Docker.
//
// Chỉ nhận request mang ngữ cảnh do MCP Gateway ký (kiểm bằng khoá công khai HQ_SERVICE_VERIFY_KEY); không
// nhận token của người dùng. Nói chuyện với Docker qua HQ_DOCKER_API_URL, nên là
// một proxy chỉ cho phép API đọc container, không phải Docker socket trực tiếp.
package main

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"os"

	"github.com/hqgroups/mcp/internal/integration/docker"
	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/platform/process"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "integration-docker",
		DefaultAddr: ":8090",
		NoDatabase:  true,
		Routes:      routes,
	}))
}

func routes(_ context.Context, mux *http.ServeMux, _ process.Deps) error {
	apiURL := os.Getenv("HQ_DOCKER_API_URL")
	if apiURL == "" {
		return errors.New("HQ_DOCKER_API_URL is required (địa chỉ proxy chỉ đọc của Docker API)")
	}
	client, err := docker.NewClient(apiURL, nil)
	if err != nil {
		return err
	}
	verifier, err := serve.VerifierFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	// Docker host này phục vụ đúng một organization.
	orgID, err := uuid.Parse(os.Getenv("HQ_DOCKER_ORG_ID"))
	if err != nil || orgID == uuid.Nil {
		return errors.New("HQ_DOCKER_ORG_ID is required (id của organization sở hữu Docker host này)")
	}
	mux.Handle("/mcp", docker.Handler(client, verifier, orgID))
	return nil
}
