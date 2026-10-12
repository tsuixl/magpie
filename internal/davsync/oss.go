package davsync

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// signOSS is OSS4 header authentication for the native append operation.
// Ordinary snapshots and usage keep their existing AWS SigV4 S3 transport.
// OSS4 differs in its bucket-qualified URI, empty query values, mandatory
// x-oss headers and key derivation; it is not an AWS service-name variant.
// https://help.aliyun.com/zh/oss/developer-reference/recommend-to-use-signature-version-4
func (s signer) signOSS(req *http.Request, bucket string, at time.Time) {
	stamp := at.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("Date", at.UTC().Format(http.TimeFormat))
	req.Header.Set("X-Oss-Date", stamp)
	req.Header.Set("X-Oss-Content-Sha256", "UNSIGNED-PAYLOAD")
	var names []string
	for name := range req.Header {
		name = strings.ToLower(name)
		if name == "content-type" || name == "content-md5" || strings.HasPrefix(name, "x-oss-") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var headers strings.Builder
	for _, name := range names {
		vs := req.Header.Values(name)
		trimmed := make([]string, len(vs))
		for i, v := range vs {
			trimmed[i] = strings.TrimSpace(v)
		}
		headers.WriteString(name + ":" + strings.Join(trimmed, ",") + "\n")
	}
	var query []string
	for name, vs := range req.URL.Query() {
		for _, v := range vs {
			p := awsEscape(name, false)
			if v != "" {
				p += "=" + awsEscape(v, false)
			}
			query = append(query, p)
		}
	}
	slices.Sort(query)
	canonical := strings.Join([]string{req.Method, awsEscape("/"+bucket+req.URL.Path, true),
		strings.Join(query, "&"), headers.String(), "", "UNSIGNED-PAYLOAD"}, "\n")
	scope := day + "/" + s.region + "/oss/aliyun_v4_request"
	toSign := "OSS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + sum([]byte(canonical))
	key := mac([]byte("aliyun_v4"+s.secret), day)
	key = mac(key, s.region)
	key = mac(key, "oss")
	key = mac(key, "aliyun_v4_request")
	req.Header.Set("Authorization", "OSS4-HMAC-SHA256 Credential="+s.id+"/"+scope+", Signature="+hex.EncodeToString(mac(key, toSign)))
}

// appendOSS uses only a recognized regional S3 endpoint's corresponding
// native OSS host, preserving the object key and internal/public endpoint.
func (s *s3) appendOSS(ctx context.Context, key string, data []byte, position int64) (*http.Response, error) {
	if ossS3Region(s.endpoint.Hostname()) == "" || s.pathStyle || position < 0 || position > math.MaxInt64-int64(len(data)) {
		return nil, fmt.Errorf("OSS append needs a regional OSS S3 endpoint and virtual-hosted addressing")
	}
	u := s.urlOf(key)
	u.Host = s.bucket + "." + strings.TrimPrefix(strings.ToLower(s.endpoint.Host), "s3.")
	u.RawQuery = fmt.Sprintf("append&position=%d", position)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	digest := md5.Sum(data)
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(digest[:]))
	s.sig.signOSS(req, s.bucket, s.now())
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		next, err := strconv.ParseInt(res.Header.Get("X-Oss-Next-Append-Position"), 10, 64)
		if err != nil || next != position+int64(len(data)) {
			res.Body.Close()
			return nil, fmt.Errorf("OSS append returned an unexpected next position; the commit outcome is unknown")
		}
	}
	return res, nil
}
