package davsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	ossJournalToken = "oss-journal-v1:"
	ossLegacyToken  = "oss-legacy-v1:"
	ossJournalTail  = 4 << 10
	ossRecordLimit  = 1024
)

type ossCommit struct {
	Schema   int    `json:"schema"`
	Snapshot string `json:"snapshot"`
	SHA256   string `json:"sha256"`
}

type ossJournal struct {
	there  bool
	length int64
	last   ossCommit
	token  string
}

func (s *s3) ossBackup() bool {
	return ossS3Region(s.endpoint.Hostname()) != "" && (s.key == folder+"/"+file || strings.HasSuffix(s.key, "/"+folder+"/"+file))
}

func (s *s3) journalKey() string { return s.key + ".journal" }
func (s *s3) snapshotKey(name string) string {
	return strings.TrimSuffix(s.key, file) + "snapshots/" + name
}

func validOSSHash(h string) bool {
	b, err := hex.DecodeString(h)
	return err == nil && len(b) == 32 && h == strings.ToLower(h)
}

func validOSSSnapshot(name string) bool {
	id, ok := strings.CutSuffix(name, ".magpie-backup")
	b, err := hex.DecodeString(id)
	return ok && err == nil && len(b) == 16 && id == strings.ToLower(id)
}

// readOSSJournal reads only the bounded tail. It never interprets a broken
// journal as empty or returns to the legacy backup after a journal was seen.
func (s *s3) readOSSJournal(ctx context.Context) (ossJournal, error) {
	res, err := s.sendTo(ctx, http.MethodGet, s.urlOf(s.journalKey()), nil, map[string]string{"Range": fmt.Sprintf("bytes=-%d", ossJournalTail), "Accept-Encoding": "identity"})
	if err != nil {
		return ossJournal{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		e := readError(res)
		if e.Code != "NoSuchBucket" {
			return ossJournal{}, nil
		}
		return ossJournal{}, s.explain("read", e)
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return ossJournal{}, s.explain("read", readError(res))
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, ossJournalTail+1))
	if err != nil {
		return ossJournal{}, err
	}
	if len(b) == 0 || len(b) > ossJournalTail || b[len(b)-1] != '\n' {
		return ossJournal{}, errors.New("the OSS sync journal is empty, too long or cut short; it was not replaced")
	}
	start, total := int64(0), int64(len(b))
	if res.StatusCode == http.StatusPartialContent {
		var end int64
		if _, err := fmt.Sscanf(res.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start < 0 || total <= 0 || start > end || end >= total || end-start+1 != int64(len(b)) || end != total-1 {
			return ossJournal{}, errors.New("the OSS sync journal has an invalid byte range")
		}
		if start > 0 {
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				return ossJournal{}, errors.New("the OSS sync journal tail has no complete record")
			}
			b = b[i+1:]
		}
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte{'\n'}), []byte{'\n'})
	var last ossCommit
	var lastLine []byte
	for _, line := range lines {
		if len(line) == 0 || len(line) > ossRecordLimit {
			return ossJournal{}, errors.New("the OSS sync journal contains an invalid record")
		}
		var record ossCommit
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil || record.Schema != 1 || !validOSSSnapshot(record.Snapshot) || !validOSSHash(record.SHA256) {
			return ossJournal{}, errors.New("the OSS sync journal record is damaged or references an invalid snapshot")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return ossJournal{}, errors.New("the OSS sync journal record contains trailing data")
		}
		last, lastLine = record, line
	}
	return ossJournal{there: true, length: total, last: last, token: ossJournalToken + strconv.FormatInt(total, 10) + ":" + sum(lastLine)}, nil
}

func ossLegacyVersion(data []byte, v version) version {
	if data == nil {
		return version{}
	}
	return version{ETag: ossLegacyToken + base64.RawURLEncoding.EncodeToString([]byte(v.ETag)) + ":" + sum(data)}
}

func (s *s3) getOSSBackup(ctx context.Context, have version) ([]byte, version, error) {
	return s.readOSSBackup(ctx, have, false)
}

func (s *s3) readOSSBackup(ctx context.Context, have version, fresh bool) ([]byte, version, error) {
	j, err := s.readOSSJournal(ctx)
	if err != nil {
		return nil, version{}, err
	}
	if !j.there {
		if strings.HasPrefix(have.ETag, ossJournalToken) {
			return nil, version{}, errors.New("the OSS sync journal is missing after an earlier commit; the old backup was not used")
		}
		data, v, err := s.getObject(ctx, version{})
		if err != nil {
			return nil, version{}, err
		}
		v = ossLegacyVersion(data, v)
		if !fresh && data != nil && v.ETag == have.ETag {
			return nil, have, errNotModified
		}
		return data, v, nil
	}
	if !fresh && j.token == have.ETag {
		return nil, have, errNotModified
	}
	copy := *s
	copy.key = s.snapshotKey(j.last.Snapshot)
	data, _, err := copy.getObject(ctx, version{})
	if err != nil {
		return nil, version{}, err
	}
	if data == nil || sum(data) != j.last.SHA256 {
		return nil, version{}, errors.New("the OSS sync journal's snapshot is missing or damaged; the old backup was not used")
	}
	return data, version{ETag: j.token}, nil
}

func (s *s3) putOSSBackup(ctx context.Context, data []byte, have string) (version, error) {
	j, err := s.readOSSJournal(ctx)
	if err != nil {
		return version{}, err
	}
	if j.there {
		if j.token != have {
			return version{}, errChanged
		}
	} else {
		if strings.HasPrefix(have, ossJournalToken) {
			return version{}, errors.New("the OSS sync journal disappeared after an earlier commit")
		}
		legacy, v, err := s.getObject(ctx, version{})
		if err != nil {
			return version{}, err
		}
		if ossLegacyVersion(legacy, v).ETag != have {
			return version{}, errChanged
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return version{}, err
	}
	name := hex.EncodeToString(id[:]) + ".magpie-backup"
	key := s.snapshotKey(name)
	res, err := s.sendTo(ctx, http.MethodPut, s.urlOf(key), data, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return version{}, fmt.Errorf("uploading an OSS sync snapshot: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		e := readError(res)
		res.Body.Close()
		return version{}, s.explain("write", e)
	}
	res.Body.Close()
	record, err := json.Marshal(ossCommit{Schema: 1, Snapshot: name, SHA256: sum(data)})
	if err != nil {
		return version{}, err
	}
	line := append(append([]byte{}, record...), '\n')
	res, err = s.appendOSS(ctx, s.journalKey(), line, j.length)
	if err != nil {
		// A missing response might follow a successful append. Keep the
		// snapshot: deleting it could destroy an already committed backup.
		return version{}, fmt.Errorf("committing the OSS sync snapshot (outcome unknown; snapshot kept): %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return version{ETag: ossJournalToken + strconv.FormatInt(j.length+int64(len(line)), 10) + ":" + sum(record)}, nil
	}
	e := readError(res)
	if e.Status == http.StatusConflict && e.Code == "PositionNotEqualToLength" {
		// The service definitively rejected this record. Only this writer's
		// fresh, uncommitted snapshot can be removed, without an owner check.
		cleanup, cancel := context.WithTimeout(context.Background(), metaLimit)
		defer cancel()
		if r, err := s.sendTo(cleanup, http.MethodDelete, s.urlOf(key), nil, nil); err == nil {
			r.Body.Close()
		}
		return version{}, errChanged
	}
	return version{}, fmt.Errorf("committing the OSS sync snapshot: %s (snapshot kept)", e)
}
