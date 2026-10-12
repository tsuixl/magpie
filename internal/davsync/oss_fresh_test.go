package davsync

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/backup"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func ossFreshFixture(t *testing.T) (Config, *s3, *fakeS3, state) {
	t.Helper()
	newComputer(t).use(t)
	s, f := newOSSJournalFixture(t)
	before := syncClient
	syncClient = s.client
	t.Cleanup(func() { syncClient = before })
	no := false
	c := Config{URL: "s3://bkt/team x+y", Endpoint: ossEndpoint, User: f.id, Password: f.secret,
		Passphrase: "test passphrase", Keys: true, Agents: false, Library: &no}
	if err := Configure(c); err != nil {
		t.Fatal(err)
	}
	legacy := ossTestBundle(t, "legacy", "light")
	f.store(s.key, legacy)
	_, old, err := s.get(context.Background(), version{})
	if err != nil {
		t.Fatal(err)
	}
	current := ossTestBundle(t, "current", "dark")
	v, err := s.put(context.Background(), current, old.ETag)
	if err != nil {
		t.Fatal(err)
	}
	b, err := backup.Open(current, c.Passphrase)
	if err != nil {
		t.Fatal(err)
	}
	st := state{Key: stateKey(c), Server: v, Sum: sum(current), Local: hashes(b), Remote: hashes(b)}
	saveState(st)
	if err := provider.Save(provider.Provider{ID: "synthetic", Name: "keep local", Chat: "https://synthetic.invalid/v1", Key: "dummy-key"}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{Theme: "system"}); err != nil {
		t.Fatal(err)
	}
	return c, s, f, st
}

func TestOSSFreshPathsRejectDisappearedJournal(t *testing.T) {
	for _, operation := range []string{"restore", "upload", "cache miss"} {
		t.Run(operation, func(t *testing.T) {
			c, s, f, st := ossFreshFixture(t)
			if operation == "cache miss" {
				base := s.client.Transport
				removed := false
				syncClient = &http.Client{Transport: ossTestTransport(func(r *http.Request) (*http.Response, error) {
					res, err := base.RoundTrip(r)
					if err == nil && !removed && r.Method == http.MethodGet && r.URL.Path == s.urlOf(s.journalKey()).Path {
						removed = true
						f.mu.Lock()
						delete(f.objects, s.journalKey())
						delete(f.etags, s.journalKey())
						f.mu.Unlock()
					}
					return res, err
				})}
			} else {
				delete(f.objects, s.journalKey())
				delete(f.etags, s.journalKey())
				if operation == "upload" {
					f.store(s.key, []byte{})
				}
			}
			var err error
			switch operation {
			case "restore":
				_, err = Restore(context.Background())
			case "upload":
				err = Upload(context.Background())
			case "cache miss":
				err = syncOnce(context.Background(), c, &st)
			}
			if err == nil || !strings.Contains(err.Error(), "journal") || !strings.Contains(err.Error(), "missing") {
				t.Fatalf("%s accepted a vanished committed journal or failed for the wrong reason: %v", operation, err)
			}
			p, err := provider.Find("synthetic")
			if err != nil || p.Name != "keep local" {
				t.Fatal("vanished journal imported legacy providers")
			}
			if _, exists := f.objects[s.journalKey()]; exists {
				t.Fatal("vanished journal was reinitialized")
			}
		})
	}
}

func TestOSSFreshRestoreForcesSnapshotAndScopesState(t *testing.T) {
	t.Run("same token still reads the committed snapshot", func(t *testing.T) {
		_, _, _, _ = ossFreshFixture(t)
		if _, err := Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		p, err := provider.Find("synthetic")
		if err != nil || p.Name != "current" || settings.Load().Theme != "dark" {
			t.Fatal("fresh restore did not fetch the current snapshot")
		}
	})
	t.Run("different target state does not block legacy migration", func(t *testing.T) {
		c, s, f, st := ossFreshFixture(t)
		other := c
		other.URL = "s3://bkt/different-target"
		st.Key = stateKey(other)
		saveState(st)
		delete(f.objects, s.journalKey())
		delete(f.etags, s.journalKey())
		if _, err := Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		p, err := provider.Find("synthetic")
		if err != nil || p.Name != "legacy" {
			t.Fatal("unrelated state incorrectly prevented a legacy read")
		}
	})
	for _, bad := range []string{"missing", "damaged"} {
		t.Run("same token validates "+bad+" snapshot", func(t *testing.T) {
			_, s, f, _ := ossFreshFixture(t)
			j, err := s.readOSSJournal(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			key := s.snapshotKey(j.last.Snapshot)
			if bad == "missing" {
				delete(f.objects, key)
				delete(f.etags, key)
			} else {
				f.store(key, []byte("damaged"))
			}
			if _, err := Restore(context.Background()); err == nil || !strings.Contains(err.Error(), "snapshot") {
				t.Fatalf("fresh read bypassed snapshot validation: %v", err)
			}
		})
	}
}
