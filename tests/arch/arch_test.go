// Package arch_test kiểm tra luật phụ thuộc giữa các bounded context (ADR-0001)
// mà depguard không diễn đạt gọn được.
package arch_test

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/hqgroups/mcp"

type pkg struct {
	ImportPath string
	Imports    []string
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-json=ImportPath,Imports", module+"/...")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list không trả về package nào")
	}
	return pkgs
}

// contextOf trả về bounded context của một package dưới internal/:
// "agent", "mcp", "cost", "platform/authz", "shared"... Chuỗi rỗng nếu không thuộc internal/.
func contextOf(importPath string) string {
	rest, ok := strings.CutPrefix(importPath, module+"/internal/")
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "/", 3)
	// platform/ gom nhiều bounded context (identity, tenancy, authz, secrets...):
	// mỗi thư mục con là một context riêng khi xét luật store.
	if parts[0] == "platform" && len(parts) > 1 {
		return "platform/" + parts[1]
	}
	return parts[0]
}

func hasSegment(importPath, seg string) bool {
	for _, s := range strings.Split(importPath, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// Context khác không được import package lưu trữ (`store`) của một context.
func TestNoCrossContextStoreImports(t *testing.T) {
	for _, p := range listPackages(t) {
		from := contextOf(p.ImportPath)
		if from == "" || from == "testutil" {
			continue
		}
		for _, imp := range p.Imports {
			to := contextOf(imp)
			if to == "" || to == from || !hasSegment(imp, "store") {
				continue
			}
			t.Errorf("%s import package lưu trữ của context khác: %s", p.ImportPath, imp)
		}
	}
}

// shared/ không phụ thuộc vào bất kỳ bounded context nào.
func TestSharedImportsNoContext(t *testing.T) {
	for _, p := range listPackages(t) {
		if contextOf(p.ImportPath) != "shared" {
			continue
		}
		for _, imp := range p.Imports {
			if to := contextOf(imp); to != "" && to != "shared" {
				t.Errorf("%s (shared) import context %q: %s", p.ImportPath, to, imp)
			}
		}
	}
}

// Code sản phẩm không import package hỗ trợ test.
func TestProductionCodeDoesNotImportTestutil(t *testing.T) {
	for _, p := range listPackages(t) {
		if strings.HasPrefix(p.ImportPath, module+"/tests/") || contextOf(p.ImportPath) == "testutil" {
			continue
		}
		for _, imp := range p.Imports {
			if contextOf(imp) == "testutil" {
				t.Errorf("%s import package test: %s", p.ImportPath, imp)
			}
		}
	}
}

// deps trả về mọi package mà một binary link vào (phụ thuộc bắc cầu).
func deps(t *testing.T, target string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", module+"/"+target).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", target, err)
	}
	return strings.Fields(string(out))
}

// TestProcessBoundaries khoá bảng "ba process" (overview §3) ở mức binary: một process
// không link vào code mà nó không được phép dùng.
func TestProcessBoundaries(t *testing.T) {
	for _, c := range []struct {
		binary string
		reason string
		deny   []string
	}{
		{"cmd/worker", "worker không có quyền nào với secret, không gọi thẳng MCP server hay provider có khoá",
			[]string{module + "/internal/platform/secrets", module + "/internal/mcp/gateway", module + "/internal/integration/",
				module + "/internal/ai/provider/anthropic", module + "/internal/gatewayserver", module + "/internal/apiserver",
				// Hàng đợi api-internal (lịch chạy của workflow, ADR-0015) chỉ process api poll được.
				module + "/internal/workflow/definitions"}},
		{"cmd/gateway", "gateway không chứa logic chạy Agent và không phụ thuộc Temporal",
			[]string{"go.temporal.io/", module + "/internal/agent/runtime/durable", module + "/internal/workerserver", module + "/internal/runtimewire",
				module + "/internal/workflow/definitions"}},
		{"cmd/api", "api không giải mã secret bằng khoá gốc và không chứa code của Gateway",
			[]string{module + "/internal/gatewayserver", module + "/internal/integration/", module + "/internal/workerserver"}},
	} {
		for _, dep := range deps(t, c.binary) {
			for _, deny := range c.deny {
				if dep == deny || strings.HasPrefix(dep, deny) && (strings.HasSuffix(deny, "/") || strings.HasPrefix(dep, deny+"/")) {
					t.Errorf("%s link vào %s: %s", c.binary, dep, c.reason)
				}
			}
		}
	}
}

// TestTemporalSDKIsIsolated: Temporal SDK chỉ được import ở phần nối vòng lặp với
// Temporal và các composition root dùng nó (rủi ro R4, giống cách MCP SDK bị cô lập).
// TestOrchestratorIsPure: phần thuần của Orchestrator (kiểm kế hoạch, bộ lập lịch) là
// thân của workflow cha trên Temporal. Nó không được phụ thuộc package nào của dự án,
// không chạm database, mạng hay đồng hồ.
func TestOrchestratorIsPure(t *testing.T) {
	found := false
	for _, p := range listPackages(t) {
		if p.ImportPath != module+"/internal/agent/orchestrator" {
			continue
		}
		found = true
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, module+"/internal/") || strings.HasPrefix(imp, "github.com/jackc/pgx") ||
				imp == "database/sql" || imp == "net/http" || imp == "os" || imp == "time" || strings.HasPrefix(imp, "go.temporal.io") {
				t.Errorf("%s import %s: package này phải thuần", p.ImportPath, imp)
			}
		}
	}
	if !found {
		t.Fatal("không thấy package internal/agent/orchestrator")
	}
}

func TestTemporalSDKIsIsolated(t *testing.T) {
	// internal/workflow/definitions/schedule: lịch chạy của workflow trên Temporal Schedules (ADR-0015).
	allowed := []string{module + "/internal/agent/runtime/durable", module + "/internal/workerserver",
		module + "/internal/workflow/definitions/schedule", module + "/tests/"}
	for _, p := range listPackages(t) {
		ok := false
		for _, a := range allowed {
			if p.ImportPath == a || strings.HasPrefix(p.ImportPath, a) {
				ok = true
			}
		}
		if ok {
			continue
		}
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, "go.temporal.io/") {
				t.Errorf("%s import %s: Temporal SDK chỉ được dùng trong %v", p.ImportPath, imp, allowed)
			}
		}
	}
}
