package davsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
)

const ossEndpoint = "https://s3.oss-cn-hangzhou.aliyuncs.com"

func TestOSSRegionalEndpointVariants(t *testing.T) {
	for _, region := range []string{"cn-wuhan-lr", "cn-shanghai-finance-1"} {
		for _, internal := range []bool{false, true} {
			host := "s3.oss-" + region
			if internal {
				host += "-internal"
			}
			host += ".aliyuncs.com"
			for _, configured := range []string{"", region} {
				cfg := Config{URL: "s3://test-bucket/backups", Endpoint: "https://" + host, Region: configured}
				s, err := newS3(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if s.sig.region != region || cfg.target() != "oss" || !s.ossBackup() {
					t.Errorf("%s configured_region=%q: region=%q target=%q journal=%v; valid OSS endpoints must use their region and journal", host, configured, s.sig.region, cfg.target(), s.ossBackup())
				}
			}
		}
	}
	for _, host := range []string{"s3.oss-accelerate.aliyuncs.com", "oss-cn-wuhan-lr.aliyuncs.com", "s3.oss-cn-wuhan-lr.aliyuncs.com.invalid", "s3.oss-cn--wuhan.aliyuncs.com"} {
		if got := ossS3Region(host); got != "" {
			t.Errorf("non-regional endpoint %q inferred %q", host, got)
		}
	}
}

func TestOSSS3Address(t *testing.T) {
	for _, endpoint := range []string{ossEndpoint, "s3.oss-cn-hangzhou.aliyuncs.com/"} {
		s, err := newS3(Config{URL: "s3://backup-bucket/team x+y", Endpoint: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.objectURL().String(); got != "https://backup-bucket.s3.oss-cn-hangzhou.aliyuncs.com/team%20x%2By/magpie/magpie.magpie-backup" || s.sig.region != "cn-hangzhou" || s.pathStyle {
			t.Fatalf("OSS address %s, region %s, path-style %v", got, s.sig.region, s.pathStyle)
		}
	}
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{URL: "s3://backup-bucket", Endpoint: ossEndpoint, Region: "us-east-1"}, "region cn-hangzhou"},
		{Config{URL: "s3://backup-bucket", Endpoint: ossEndpoint, PathStyle: true}, "virtual-hosted"},
		{Config{URL: "s3://backup.bucket", Endpoint: ossEndpoint}, "without dots"},
	} {
		if err := Check(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Check(%+v) = %v, want %s", tc.cfg, err, tc.want)
		}
	}
	// Native endpoints and custom domains keep the generic S3 behavior;
	// the preset must not change old configurations on an untested claim.
	s, err := newS3(Config{URL: "s3://backup-bucket", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com", PathStyle: true})
	if err != nil || s.sig.region != "us-east-1" || !s.pathStyle {
		t.Fatalf("generic endpoint changed: %+v, %v", s, err)
	}
	for endpoint, want := range map[string]string{
		"https://s3.oss-cn-hangzhou-internal.aliyuncs.com": "cn-hangzhou",
		"https://s3.oss-accelerate.aliyuncs.com":           "us-east-1",
		"https://s3.oss-cn-hangzhou.aliyuncs.com.invalid":  "us-east-1",
		"https://s3.oss-invalid.aliyuncs.com":              "us-east-1",
	} {
		s, err := newS3(Config{URL: "s3://backup-bucket", Endpoint: endpoint})
		if err != nil || s.sig.region != want {
			t.Errorf("endpoint %s: %+v, %v, want region %s", endpoint, s, err, want)
		}
	}
}

func TestOSSS3KeepsThreeTargets(t *testing.T) {
	newComputer(t).use(t)
	dav := Server{URL: "https://dav.example.com/dav/", User: "me", Password: "dav-password"}
	s3 := Config{URL: "s3://r2-bucket/team", Endpoint: "https://acct.r2.cloudflarestorage.com", User: "R2KEY", Password: "r2-secret", Passphrase: "correct horse", Other: &dav}
	// A legacy file with Other, and no Saved, migrates without losing either.
	b, _ := json.Marshal(s3)
	if err := os.MkdirAll(path(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("sync.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	oss := Config{URL: "s3://oss-bucket/team", Endpoint: ossEndpoint, User: "OSSKEY", Password: "oss-secret"}
	if err := Configure(oss); err != nil {
		t.Fatal(err)
	}
	c, _ := Load()
	if c.Saved["s3"].Password != "r2-secret" || c.Saved["webdav"].Password != "dav-password" || c.Other == nil || c.Other.Password != "dav-password" {
		t.Fatalf("legacy servers lost: %+v", c)
	}
	view, _ := json.Marshal(Status())
	for _, secret := range []string{"r2-secret", "oss-secret", "dav-password", "correct horse"} {
		if strings.Contains(string(view), secret) {
			t.Fatalf("view leaked %s: %s", secret, view)
		}
	}
	for _, next := range []Config{dav.config(), s3, oss, dav.config(), oss, s3} {
		want := next.Password
		next.Password, next.Passphrase = "", ""
		// A form cannot inject inactive credentials.
		next.Saved = map[string]Server{"injected": {URL: "https://injected.invalid", Password: "injected-secret"}}
		if kept, needed := SavedPassword(next); !kept || needed {
			t.Fatalf("saved target not found: %+v, kept %v needed %v", next, kept, needed)
		}
		if err := Configure(next); err != nil {
			t.Fatal(err)
		}
		got, _ := Load()
		if got.Password != want || len(got.Saved) != 2 || got.Saved["injected"].URL != "" {
			t.Fatalf("target not restored: %+v", got)
		}
	}
	oss.Endpoint = "https://s3.oss-cn-shanghai.aliyuncs.com"
	oss.Password = ""
	if kept, needed := SavedPassword(oss); kept || !needed {
		t.Fatalf("saved OSS secret reused for another region: kept %v needed %v", kept, needed)
	}
}

type ossTransport struct {
	to   *url.URL
	base http.RoundTripper
}

// Only route to the local fixture; keep the original Host and request URI
// so the fixture validates the signature of the actual OSS target.
func (r ossTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = r.to.Scheme, r.to.Host
	next.URL, next.Host = &u, req.URL.Host
	return r.base.RoundTrip(next)
}

func ossClient(t *testing.T, s *s3, srv *httptest.Server) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.client = &http.Client{Transport: ossTransport{u, srv.Client().Transport}}
}

func TestOSSS3Requests(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			f, srv := newFakeS3(t)
			f.region, f.virtual, f.upperETag, f.noCond = "cn-hangzhou", true, true, fallback
			// Literal error from OSS's native conditional-header documentation,
			// not evidence that the S3 endpoint supports conditional PUT.
			f.conditionError = `<Error><Code>NotImplemented</Code><Message>A header you provided implies functionality that is not implemented.</Message><Header>If-Match</Header></Error>`
			cfg := f.config(srv)
			cfg.Endpoint = ossEndpoint
			s, err := newS3(cfg)
			if err != nil {
				t.Fatal(err)
			}
			where := s.endpoint.String() + " " + s.bucket
			unconditional.Delete(where)
			t.Cleanup(func() { unconditional.Delete(where) })
			ossClient(t, s, srv)
			ctx := context.Background()
			v, err := s.putObject(ctx, []byte("sealed backup one"), "")
			if err != nil || v.ETag == "" || v.ETag != strings.ToUpper(v.ETag) {
				t.Fatalf("first write: %+v, %v", v, err)
			}
			data, got, err := s.getObject(ctx, version{})
			if err != nil || string(data) != "sealed backup one" || got.ETag != v.ETag {
				t.Fatalf("read: %q, %+v, %v", data, got, err)
			}
			if _, _, err := s.getObject(ctx, got); !errors.Is(err, errNotModified) {
				t.Fatalf("conditional read: %v", err)
			}
			v2, err := s.putObject(ctx, []byte("sealed backup two"), v.ETag)
			if err != nil || v2.ETag == v.ETag {
				t.Fatalf("update: %+v, %v", v2, err)
			}
			if _, err := s.putObject(ctx, []byte("stale backup"), v.ETag); !errors.Is(err, errChanged) {
				t.Fatalf("stale write: %v", err)
			}
			if _, err := s.putObject(ctx, []byte("first again"), ""); !errors.Is(err, errChanged) {
				t.Fatalf("create over existing: %v", err)
			}
			if !fallback {
				start, done := make(chan struct{}), make(chan error, 2)
				for _, body := range []string{"concurrent one", "concurrent two"} {
					go func() {
						<-start
						_, err := s.putObject(ctx, []byte(body), v2.ETag)
						done <- err
					}()
				}
				close(start)
				one, two := <-done, <-done
				if !(one == nil && errors.Is(two, errChanged) || two == nil && errors.Is(one, errChanged)) {
					t.Fatalf("concurrent conditional writes: %v, %v", one, two)
				}
			}
			if fallback && !slices.Contains(f.log, "HEAD") || !fallback && !slices.Contains(f.log, "PUT "+v.ETag) {
				t.Fatalf("write conditions not exercised: %v", f.log)
			}
			const usageName = "0123456789abcdef-2026-10-12.magpie-usage"
			if err := s.write(ctx, usageName, []byte("sealed usage")); err != nil {
				t.Fatal(err)
			}
			if b, err := s.read(ctx, usageName); err != nil || string(b) != "sealed usage" {
				t.Fatalf("usage read: %q, %v", b, err)
			}
			if err := s.remove(ctx, usageName); err != nil {
				t.Fatal(err)
			}
			if _, there := f.objects[s.usageKey(usageName)]; there {
				t.Fatal("usage remove left the remote object")
			}
		})
	}
}

func TestOSSS3UsageAndErrors(t *testing.T) {
	f := &fakeS3{id: "AKIDTEST", secret: "secret", region: "cn-hangzhou"}
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "backup-bucket.s3.oss-cn-hangzhou.aliyuncs.com" || !f.signed(r, nil) {
			t.Errorf("unsigned OSS request: %s %s", r.Host, r.URL)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		requests = append(requests, r.URL.RequestURI())
		if q.Get("list-type") != "2" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error><Code>InvalidAccessKeyId</Code><Message>The OSS Access Key Id you provided does not exist in our records.</Message></Error>`)
			return
		}
		next := ""
		if q.Get("continuation-token") == "" {
			next = "next+/="
		} else if q.Get("continuation-token") != "next+/=" {
			t.Errorf("continuation token changed: %q", q.Get("continuation-token"))
		}
		fmt.Fprintf(w, `<ListBucketResult><Contents><Key>%s%d.magpie-usage</Key><ETag>"ABCDEF%d"</ETag></Contents><IsTruncated>%t</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>`, q.Get("prefix"), len(requests), len(requests), next != "", next)
	}))
	t.Cleanup(srv.Close)
	s, err := newS3(Config{URL: "s3://backup-bucket/team x+y", Endpoint: ossEndpoint, User: f.id, Password: f.secret})
	if err != nil {
		t.Fatal(err)
	}
	ossClient(t, s, srv)
	got, err := s.list(context.Background())
	if err != nil || len(got) != 2 || got["2.magpie-usage"] != `"ABCDEF2"` || len(requests) != 2 {
		t.Fatalf("listing: %v, %v, requests %v", got, err, requests)
	}
	if _, _, err := s.get(context.Background(), version{}); err == nil || !strings.Contains(err.Error(), "refused the access key") {
		t.Fatalf("OSS access-key error: %v", err)
	}
}
