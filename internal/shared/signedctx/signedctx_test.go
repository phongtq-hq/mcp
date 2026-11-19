package signedctx_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func TestSignVerify(t *testing.T) {
	key, err := signedctx.ParseKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := signedctx.NewSigner(key)
	other, _ := signedctx.NewSigner([]byte(strings.Repeat("x", 32)))
	now := time.Unix(1_800_000_000, 0)
	c := signedctx.Context{Kind: signedctx.KindCall, Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString()}
	tok := s.Sign(c, now)

	got, err := s.Verify(tok, "docker", now.Add(time.Minute))
	if err != nil || got.OrgID != c.OrgID || got.ProjectID != c.ProjectID || got.Initiator != c.Initiator {
		t.Fatalf("verify: %+v %v", got, err)
	}
	payload, sig, _ := strings.Cut(tok, ".")
	for name, tc := range map[string]struct {
		value, aud string
		at         time.Time
		signer     *signedctx.Signer
	}{
		"đích khác":             {tok, "kubernetes", now, s},
		"hết hạn":               {tok, "docker", now.Add(signedctx.DefaultTTL + time.Second), s},
		"khoá khác":             {tok, "docker", now, other},
		"sửa payload":           {payload + "x." + sig, "docker", now, s},
		"bỏ chữ ký":             {payload + ".", "docker", now, s},
		"rỗng":                  {"", "docker", now, s},
		"phát hành ở tương lai": {s.Sign(c, now.Add(time.Hour)), "docker", now, s},
	} {
		if _, err := tc.signer.Verify(tc.value, tc.aud, tc.at); err == nil {
			t.Errorf("%s: phải bị từ chối", name)
		}
	}
	// Thiếu org / project / initiator không bao giờ hợp lệ.
	for _, bad := range []signedctx.Context{
		{Kind: signedctx.KindCall, Audience: "docker", ProjectID: uuid.New(), Initiator: "u"},
		{Kind: signedctx.KindCall, Audience: "docker", OrgID: uuid.New(), Initiator: "u"},
		{Kind: signedctx.KindCall, Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New()},
		{Kind: "", Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New(), Initiator: "u"},
		{Kind: "admin", Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New(), Initiator: "u"},
	} {
		if _, err := s.Verify(s.Sign(bad, now), "docker", now); err == nil {
			t.Errorf("ngữ cảnh thiếu trường phải bị từ chối: %+v", bad)
		}
	}
	// Ngữ cảnh discovery không gắn tenant, nhưng vẫn cần đúng đích.
	disc := s.Sign(signedctx.Context{Kind: signedctx.KindDiscovery, Audience: "docker"}, now)
	if _, err := s.Verify(disc, "docker", now); err != nil {
		t.Errorf("discovery: %v", err)
	}
	if _, err := s.Verify(disc, "other", now); err == nil {
		t.Error("discovery sai đích phải bị từ chối")
	}
	for _, k := range []string{"", "abcd", strings.Repeat("zz", 32), strings.Repeat("ab", 31)} {
		if _, err := signedctx.ParseKey(k); err == nil {
			t.Errorf("khoá %q phải bị từ chối", k)
		}
	}
}
