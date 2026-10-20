package registry

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Bpoe/dsc-registry-sync/internal/jsondoc"
)

const (
	MaxReportBytes = 16 << 20
	// Reserve more than the maximum wrapper overhead (including a 64-byte name).
	MaxResultBytes = MaxReportBytes - 512
)

var ErrInvalidReport = errors.New("invalid report name, revision, size or JSON object")

// Report owns an immutable request body: no encoding/json roundtrip may change
// the nested envelope, even on the first request or an ambiguous HTTP failure.
type Report struct {
	name, revision, id string
	body               []byte
}

func (r *Report) Name() string     { return r.name }
func (r *Report) Revision() string { return r.revision }
func (r *Report) ID() string       { return r.id }

func NewReport(name, revision string, result []byte) (*Report, error) {
	value := bytes.TrimSpace(result)
	if !ValidName(name) || !validRevision(revision) || len(result) > MaxResultBytes ||
		len(value) == 0 || value[0] != '{' {
		return nil, ErrInvalidReport
	}
	id := uploadID(name, revision, result)
	body := fmt.Appendf(nil, `{"configurationName":%q,"configurationRevision":%q,"uploadId":%q,"result":`,
		name, revision, id)
	body = append(body, result...)
	body = append(body, '}')
	// Validate the wrapper too: its extra nesting counts toward the server limit.
	if len(body) > MaxReportBytes || jsondoc.Validate(body) != nil {
		return nil, ErrInvalidReport
	}
	return &Report{name: name, revision: revision, id: id, body: body}, nil
}

func uploadID(name, revision string, result []byte) string {
	// RFC 9562 UUIDv5, namespace 4e70e5f8-87c8-5e72-90bb-645fb9c79a04.
	// Name bytes: uint32-BE length + ASCII name, uint32-BE length + ASCII
	// revision, then the 32-byte SHA-256 of the exact result file (including LF).
	namespace := [16]byte{0x4e, 0x70, 0xe5, 0xf8, 0x87, 0xc8, 0x5e, 0x72, 0x90, 0xbb, 0x64, 0x5f, 0xb9, 0xc7, 0x9a, 0x04}
	h := sha1.New() // SHA-1 is required by UUIDv5; content is first SHA-256 hashed.
	h.Write(namespace[:])
	for _, part := range []string{name, revision} {
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(part))))
		h.Write([]byte(part))
	}
	digest := sha256.Sum256(result)
	h.Write(digest[:])
	id := h.Sum(nil)[:16]
	id[6] = id[6]&0x0f | 0x50
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

type ReportResponse struct {
	StatusCode int
	Header     http.Header
	Permanent  bool // unchanged content must not be blindly retried with a new ID
}

func (c *Client) Upload(ctx context.Context, report *Report) (ReportResponse, error) {
	if report == nil || len(report.body) == 0 {
		return ReportResponse{Permanent: true}, ErrInvalidReport
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.reports, bytes.NewReader(report.body))
	if err != nil {
		return ReportResponse{}, errors.New("cannot construct Registry report request")
	}
	request.Header.Set("Authorization", "ApiKey "+c.key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	res, err := c.do(request)
	if err != nil {
		return ReportResponse{}, err
	}
	defer res.Body.Close()
	// Receipts and error bodies are not local state and may contain secrets.
	// A bounded drain permits connection reuse without retaining or logging them.
	// Status is authoritative even if the optional receipt body is interrupted.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	response := ReportResponse{StatusCode: res.StatusCode, Header: res.Header}
	switch res.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return response, nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		// Credentials can recover after a server-side change; never discard inputs.
	default:
		response.Permanent = res.StatusCode < 500
	}
	return response, fmt.Errorf("Registry report returned HTTP %d", res.StatusCode)
}
