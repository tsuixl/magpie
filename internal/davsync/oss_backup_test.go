package davsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/backup"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func newOSSJournalFixture(t *testing.T) (*s3, *fakeS3) {
	t.Helper()
	f, srv := newFakeS3(t)
	f.virtual, f.noCond, f.upperETag, f.region = true, true, true, "cn-hangzhou"
	c := f.config(srv)
	c.Endpoint = ossEndpoint
	s, err := newS3(c)
	if err != nil {
		t.Fatal(err)
	}
	ossClient(t, s, srv)
	return s, f
}

func ossTestBundle(t *testing.T, name, theme string) []byte {
	t.Helper()
	b, err := backup.Seal(backup.Bundle{Version: backup.BundleVersion, Created: time.Now(), Keys: true,
		Providers: []provider.Provider{{ID: "synthetic", Name: name, Chat: "https://synthetic.invalid/v1", Key: "dummy-key"}},
		Settings:  &settings.Settings{Theme: theme}}, "test passphrase")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type ossTestTransport func(*http.Request) (*http.Response, error)

func (f ossTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOSSJournalConcurrentCommitAndMerge(t *testing.T) {
	s, f := newOSSJournalFixture(t)
	// This checks commit ordering, not speed. The test lifecycle and the
	// package's -timeout bound it even when race instrumentation or load
	// makes the repeated sealing and HTTP operations take longer.
	ctx := t.Context()
	base := ossTestBundle(t, "base", "light")
	v, err := s.put(ctx, base, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.get(ctx, v); !errors.Is(err, errNotModified) {
		t.Fatalf("unchanged: %v", err)
	}
	previous := s.client.Transport
	var mu sync.Mutex
	arrived := 0
	ready := make(chan struct{})
	gate := func(r *http.Request) error {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(ready)
		}
		mu.Unlock()
		select {
		case <-ready:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}
	s.client.Transport = ossTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			if err := gate(r); err != nil {
				return nil, err
			}
		}
		res, err := previous.RoundTrip(r)
		// The old HEAD/PUT implementation must fail this same test
		// deterministically, instead of winning through handler timing.
		if err == nil && r.Method == http.MethodHead && r.URL.Path == s.objectURL().Path && res.StatusCode == http.StatusOK {
			if err := gate(r); err != nil {
				res.Body.Close()
				return nil, err
			}
		}
		return res, err
	})
	a, b := ossTestBundle(t, "writer-a", "light"), ossTestBundle(t, "base", "dark")
	type result struct {
		id  int
		err error
	}
	done := make(chan result, 2)
	for id, data := range [][]byte{a, b} {
		go func() { _, err := s.put(ctx, data, v.ETag); done <- result{id, err} }()
	}
	one, two := <-done, <-done
	s.client.Transport = previous
	if !((one.err == nil && errors.Is(two.err, errChanged)) || (two.err == nil && errors.Is(one.err, errChanged))) {
		t.Fatalf("two writers: %v / %v", one.err, two.err)
	}
	loser := one
	if loser.err == nil {
		loser = two
	}
	data, latest, err := s.get(ctx, version{})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := backup.Open(data, "test passphrase")
	if err != nil {
		t.Fatal(err)
	}
	losing, err := backup.Open([][]byte{a, b}[loser.id], "test passphrase")
	if err != nil {
		t.Fatal(err)
	}
	part := "providers"
	if loser.id == 1 {
		part = "settings"
	}
	take(&merged, losing, part)
	sealed, err := backup.Seal(merged, "test passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.put(ctx, sealed, latest.ETag); err != nil {
		t.Fatal(err)
	}
	final, _, err := s.get(ctx, version{})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := backup.Open(final, "test passphrase")
	if err != nil || opened.Providers[0].Name != "writer-a" || opened.Settings.Theme != "dark" {
		t.Fatal("the retry did not preserve both writers' changes")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[s.key]; exists {
		t.Fatal("journal wrote an unprotected legacy mirror")
	}
	snapshots := 0
	for k := range f.objects {
		if strings.Contains(k, "/snapshots/") {
			snapshots++
		}
	}
	if snapshots != 3 {
		t.Fatalf("uncommitted conflict snapshot was not removed: %d snapshots", snapshots)
	}
}

func TestOSSJournalLegacyAndMissingLog(t *testing.T) {
	s, f := newOSSJournalFixture(t)
	ctx := context.Background()
	legacy := ossTestBundle(t, "legacy", "light")
	f.store(s.key, legacy)
	data, v, err := s.get(ctx, version{})
	if err != nil || string(data) != string(legacy) || !strings.HasPrefix(v.ETag, ossLegacyToken) {
		t.Fatal("legacy source was not read")
	}
	f.store(s.key, ossTestBundle(t, "changed legacy", "light"))
	if _, err := s.put(ctx, legacy, v.ETag); !errors.Is(err, errChanged) {
		t.Fatal("stale legacy source was not rejected")
	}
	_, v, err = s.get(ctx, version{})
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte{}, f.objects[s.key]...)
	committed, err := s.put(ctx, ossTestBundle(t, "new", "dark"), v.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.objects[s.key]) != string(before) {
		t.Fatal("migration replaced the legacy original")
	}
	if _, err := s.put(ctx, legacy, v.ETag); !errors.Is(err, errChanged) {
		t.Fatal("legacy expectation could write over a journal")
	}
	delete(f.objects, s.journalKey())
	if _, _, err := s.get(ctx, committed); err == nil {
		t.Fatal("missing journal silently fell back to legacy")
	}
	if _, err := s.put(ctx, legacy, committed.ETag); err == nil {
		t.Fatal("missing journal was silently recreated")
	}
}

func TestOSSJournalAmbiguousCommitKeepsSnapshot(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", afterCommit), func(t *testing.T) {
			s, f := newOSSJournalFixture(t)
			base := s.client.Transport
			s.client.Transport = ossTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost {
					return base.RoundTrip(r)
				}
				if afterCommit {
					res, err := base.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					res.Body.Close()
				}
				return nil, errors.New("simulated lost append response or crash before commit")
			})
			body := ossTestBundle(t, "unknown outcome", "dark")
			if _, err := s.put(context.Background(), body, ""); err == nil {
				t.Fatal("ambiguous append reported success")
			}
			s.client.Transport = base
			snapshots := 0
			for k := range f.objects {
				if strings.Contains(k, "/snapshots/") {
					snapshots++
				}
			}
			if snapshots != 1 {
				t.Fatal("unknown outcome deleted its possibly committed snapshot")
			}
			data, _, err := s.get(context.Background(), version{})
			if err != nil || afterCommit && string(data) != string(body) || !afterCommit && data != nil {
				t.Fatal("read did not resolve the actual committed state")
			}
		})
	}
}

func TestOSSJournalBoundedTailAndCorruption(t *testing.T) {
	s, f := newOSSJournalFixture(t)
	ctx := context.Background()
	body := ossTestBundle(t, "tail", "dark")
	if _, err := s.put(ctx, body, ""); err != nil {
		t.Fatal(err)
	}
	line := append([]byte{}, f.objects[s.journalKey()]...)
	f.store(s.journalKey(), []byte(strings.Repeat(string(line), (64<<10)/len(line)+50)))
	if got, _, err := s.get(ctx, version{}); err != nil || string(got) != string(body) {
		t.Fatalf("bounded suffix read: %v", err)
	}
	base := s.client.Transport
	for _, kind := range []string{"ignored range", "invalid range"} {
		t.Run(kind, func(t *testing.T) {
			s.client.Transport = ossTestTransport(func(r *http.Request) (*http.Response, error) {
				next := r.Clone(r.Context())
				if kind == "ignored range" {
					next.Header.Del("Range")
					s.sig.sign(next, next.Header.Get("X-Amz-Content-Sha256"), s.now())
				}
				res, err := base.RoundTrip(next)
				if err == nil && kind == "invalid range" {
					res.Header.Set("Content-Range", "bytes 100-20/5")
				}
				return res, err
			})
			if _, _, err := s.get(ctx, version{}); err == nil {
				t.Fatal("invalid or unbounded range was accepted")
			}
			s.client.Transport = base
		})
	}
	var rec ossCommit
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(line))), &rec); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"cut log", "invalid key", "bad hash", "missing snapshot"} {
		t.Run(kind, func(t *testing.T) {
			copy := rec
			switch kind {
			case "invalid key":
				copy.Snapshot = "../outside.magpie-backup"
			case "bad hash":
				copy.SHA256 = strings.Repeat("0", 64)
			case "missing snapshot":
				copy.Snapshot = strings.Repeat("a", 32) + ".magpie-backup"
			}
			b, _ := json.Marshal(copy)
			if kind != "cut log" {
				b = append(b, '\n')
			}
			f.store(s.journalKey(), b)
			f.store(s.key, body)
			if _, _, err := s.get(ctx, version{}); err == nil {
				t.Fatal("damaged journal fell back or returned a snapshot")
			}
		})
	}
}

func TestOSSArchiveStillUsesRawS3(t *testing.T) {
	s, f := newOSSJournalFixture(t)
	s.key = "magpie/requests/2026-10-12/request.gz"
	f.store(s.key, []byte("archive"))
	data, _, err := s.get(context.Background(), version{})
	if err != nil || string(data) != "archive" {
		t.Fatal("archive lookup entered the OSS journal")
	}
}

func TestOSSJournalNextPositionAndRedirectFailClosed(t *testing.T) {
	for _, fault := range []string{"missing position", "wrong position", "redirect"} {
		t.Run(fault, func(t *testing.T) {
			s, f := newOSSJournalFixture(t)
			called := false
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }))
			defer redirect.Close()
			base := s.client.Transport
			s.client.Transport = ossTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && fault == "redirect" {
					return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {redirect.URL}}, Body: http.NoBody, Request: r}, nil
				}
				res, err := base.RoundTrip(r)
				if err == nil && r.Method == http.MethodPost {
					if fault == "missing position" {
						res.Header.Del("X-Oss-Next-Append-Position")
					} else {
						res.Header.Set("X-Oss-Next-Append-Position", "1")
					}
				}
				return res, err
			})
			if _, err := s.put(context.Background(), []byte("snapshot"), ""); err == nil {
				t.Fatal("uncertain append outcome was accepted")
			}
			if called {
				t.Fatal("native append followed a redirect")
			}
			snapshots := 0
			for k := range f.objects {
				if strings.Contains(k, "/snapshots/") {
					snapshots++
				}
			}
			if snapshots != 1 {
				t.Fatal("uncertain commit snapshot was deleted")
			}
		})
	}
}

func TestOSSSyncNowRetriesWholeMergeAfterAppendConflict(t *testing.T) {
	newComputer(t).use(t)
	s, f := newOSSJournalFixture(t)
	oldClient := syncClient
	syncClient = s.client
	defer func() { syncClient = oldClient }()
	no := false
	cfg := Config{URL: "s3://bkt/team x+y", Endpoint: ossEndpoint, User: f.id, Password: f.secret,
		Passphrase: "test passphrase", Keys: true, Agents: false, Library: &no}
	if err := provider.Save(provider.Provider{ID: "synthetic", Name: "baseline", Chat: "https://synthetic.invalid/v1", Key: "dummy-key"}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{Theme: "light"}); err != nil {
		t.Fatal(err)
	}
	if err := Configure(cfg); err != nil {
		t.Fatal(err)
	}
	// One context spans two syncs, several Seal/Open calls and verification.
	// It is a correctness test, so do not impose a wall-clock speed budget.
	ctx := t.Context()
	if err := Now(ctx); err != nil {
		t.Fatal(err)
	}
	data, v, err := s.get(ctx, version{})
	if err != nil {
		t.Fatal(err)
	}
	competitor, err := backup.Open(data, cfg.Passphrase)
	if err != nil {
		t.Fatal(err)
	}
	competitor.Settings.Theme = "dark"
	competingData, err := backup.Seal(competitor, cfg.Passphrase)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "synthetic", Name: "local change", Chat: "https://synthetic.invalid/v1", Key: "dummy-key"}); err != nil {
		t.Fatal(err)
	}
	base := s.client.Transport
	injected := false
	syncClient = &http.Client{Transport: ossTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && !injected {
			injected = true
			if _, err := s.put(ctx, competingData, v.ETag); err != nil {
				return nil, err
			}
		}
		return base.RoundTrip(r)
	})}
	if err := Now(ctx); err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("the competing commit was not injected before append")
	}
	final, _, err := s.get(ctx, version{})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := backup.Open(final, cfg.Passphrase)
	if err != nil || opened.Providers[0].Name != "local change" || opened.Settings.Theme != "dark" || settings.Load().Theme != "dark" {
		t.Fatal("syncNow did not re-read and merge both computers' changed parts")
	}
}
