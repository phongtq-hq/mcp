package exectoken

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignVerify(t *testing.T) {
	iss, err := New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := New(bytes.Repeat([]byte("x"), 32))
	now := time.Now()
	c := Claims{OrgID: uuid.New(), ProjectID: uuid.New(), ExecutionID: uuid.New(), TaskID: uuid.New(),
		AgentVersionID: uuid.New(), InitiatorType: "user", InitiatorID: uuid.New()}
	tok, err := iss.Sign(c, now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := iss.Verify(tok, now.Add(time.Minute)); err != nil || got != c {
		t.Fatalf("verify: %+v %v", got, err)
	}
	for name, tc := range map[string]struct {
		iss *Issuer
		tok string
		at  time.Time
	}{
		"expired":   {iss, tok, now.Add(11 * time.Minute)},
		"wrong key": {other, tok, now},
		"tampered":  {iss, tok[:len(tok)-2] + "xx", now},
		"garbage":   {iss, "a.b.c", now},
		"alg none":  {iss, "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + strings.Split(tok, ".")[1] + ".", now},
		"empty":     {iss, "", now},
	} {
		if _, err := tc.iss.Verify(tc.tok, tc.at); err == nil {
			t.Errorf("%s: token được chấp nhận", name)
		}
	}
	// Thời hạn bị chặn ở MaxTTL.
	long, _ := iss.Sign(c, now, now.Add(24*time.Hour))
	if _, err := iss.Verify(long, now.Add(MaxTTL+time.Minute)); err == nil {
		t.Error("token sống quá MaxTTL")
	}
	if _, err := iss.Sign(Claims{OrgID: uuid.New()}, now, now.Add(time.Minute)); err == nil {
		t.Error("ký được token thiếu ngữ cảnh")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Error("khoá ngắn được chấp nhận")
	}
}

func TestServiceToken(t *testing.T) {
	iss, _ := New(bytes.Repeat([]byte("k"), 32))
	now := time.Now()
	c := ServiceClaims{OrgID: uuid.New(), Purpose: "mcp_discovery", Resource: uuid.New(), ActorType: "user", ActorID: uuid.New()}
	tok, err := iss.SignService(c, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := iss.VerifyService(tok, "mcp_discovery", now); err != nil || got != c {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if _, err := iss.VerifyService(tok, "other_purpose", now); err == nil {
		t.Error("token dùng được cho mục đích khác")
	}
	if _, err := iss.VerifyService(tok, "mcp_discovery", now.Add(2*time.Minute)); err == nil {
		t.Error("service token sống quá một phút")
	}
	// Hai loại token không dùng thay cho nhau.
	if _, err := iss.Verify(tok, now); err == nil {
		t.Error("service token được nhận như execution token")
	}
	exec, _ := iss.Sign(Claims{OrgID: uuid.New(), ProjectID: uuid.New(), ExecutionID: uuid.New(), TaskID: uuid.New(),
		AgentVersionID: uuid.New(), InitiatorType: "user", InitiatorID: uuid.New()}, now, now.Add(time.Minute))
	if _, err := iss.VerifyService(exec, "mcp_discovery", now); err == nil {
		t.Error("execution token được nhận như service token")
	}
}
