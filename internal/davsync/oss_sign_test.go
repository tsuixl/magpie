package davsync

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOSS4OfficialSDKVector(t *testing.T) {
	// Independent oracle: Alibaba's signer_test.go TestV4AuthHeader case 1.
	// https://github.com/aliyun/alibabacloud-oss-go-sdk-v2/blob/master/oss/signer/signer_test.go
	r, _ := http.NewRequest("PUT", "https://bucket.oss-cn-hangzhou.aliyuncs.com/1234+-/123/1.txt", nil)
	r.Header.Set("X-Oss-Head1", "value")
	r.Header.Set("Content-Type", "text/plain")
	for _, h := range []string{"abc", "ZAbc", "XYZ"} {
		r.Header.Set(h, "value")
	}
	r.URL.RawQuery = url.Values{"param1": {"value1"}, "+param1": {"value3"}, "|param1": {"value4"}, "+param2": {""}, "|param2": {""}, "param2": {""}}.Encode()
	signer{id: "ak", secret: "sk", region: "cn-hangzhou"}.signOSS(r, "bucket", time.Unix(1702743657, 0))
	_, got, _ := strings.Cut(r.Header.Get("Authorization"), "Signature=")
	if got != "e21d18daa82167720f9b1047ae7e7f1ce7cb77a31e8203a7d5f4624fa0284afe" {
		t.Fatalf("OSS4 disagrees with the official SDK vector: %s", got)
	}
}
