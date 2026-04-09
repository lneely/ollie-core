package exec

import (
	"strings"
	"testing"
	"time"
)

func newTestExecutor(t *testing.T) *Executor {
	t.Helper()
	return New(t.TempDir(), "")
}

func TestValidateCode(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		wantErr bool
	}{
		{"safe code", "echo hello", false},
		{"rm -rf /", "rm -rf /", true},
		{"rm -rf / with spaces", "rm  -rf  /", true},
		{"rm -rf / with tabs", "rm\t-rf\t/", true},
		{"rm -rf/ no space", "rm -rf/", true},
		{"rm -fr /", "rm -fr /", true},
		{"rm -fr/ no space", "rm -fr/", true},
		{"rm -rf /home", "rm -rf /home", true},
		{"rm -rf /var", "rm -rf /var", true},
		{"rm -rf /etc", "rm -rf /etc", true},
		{"rm -rf /usr", "rm -rf /usr", true},
		{"fork bomb", ":(){ :|:& };:", true},
		{"fork bomb with spaces", ": ( ) { : | : & } ; :", true},
		{"mkfs", "mkfs.ext4 /dev/sda", true},
		{"dd from device", "dd if=/dev/zero of=/dev/sda", true},
		{"device write", "echo data > /dev/sda", true},
		{"sudo", "sudo rm file", true},
		{"su", "su - root", true},
		{"etc shadow", "cat /etc/shadow", true},
		{"safe 9p", "9p read anvillm/inbox/user", false},
		{"safe jq", "echo '{}' | jq .field", false},
		{"safe rm", "rm file.txt", false},
		{"rm -rf ./", "rm -rf ./", true},
		{"rm -rf ../", "rm -rf ../", true},
		{"rm -rf .", "rm -rf .", true},
		{"rm -r -f /", "rm -r -f /", true},
		{"rm -f -r /", "rm -f -r /", true},
		{"rm --recursive --force /", "rm --recursive --force /", true},
		{"rm --force --recursive /", "rm --force --recursive /", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newTestExecutor(t).ValidateCode(tt.code)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCode() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDangerousPatternsCompile(t *testing.T) {
	if len(dangerousPatterns) == 0 {
		t.Error("dangerousPatterns should not be empty")
	}
	for i, p := range dangerousPatterns {
		if p == nil {
			t.Errorf("dangerousPatterns[%d] is nil", i)
		}
	}
}

func TestLimitedWriter(t *testing.T) {
	tests := []struct {
		name          string
		limit         int
		writes        []string
		wantTotal     int
		wantTruncated bool
	}{
		{
			name:          "under limit",
			limit:         100,
			writes:        []string{"hello", " ", "world"},
			wantTotal:     11,
			wantTruncated: false,
		},
		{
			name:          "at limit",
			limit:         5,
			writes:        []string{"hello"},
			wantTotal:     5,
			wantTruncated: false,
		},
		{
			name:          "over limit",
			limit:         5,
			writes:        []string{"hello", "world"},
			wantTotal:     5,
			wantTruncated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			lw := &limitedWriter{w: &buf, limit: tt.limit}

			for _, write := range tt.writes {
				lw.Write([]byte(write))
			}

			if lw.truncated != tt.wantTruncated {
				t.Errorf("limitedWriter truncated = %v, want %v", lw.truncated, tt.wantTruncated)
			}
			if buf.Len() != tt.wantTotal {
				t.Errorf("limitedWriter wrote %d bytes, want %d", buf.Len(), tt.wantTotal)
			}
		})
	}
}

func resetExecutor(e *Executor) {
	e.rateLimitMu.Lock()
	defer e.rateLimitMu.Unlock()
	e.validationFailures = 0
	e.lastFailure = time.Time{}
	e.blockedUntil = time.Time{}
}

func TestRateLimitCounterResetAfterWindow(t *testing.T) {
	e := newTestExecutor(t)

	for range maxFailures - 1 {
		e.recordValidationFailure()
	}

	e.rateLimitMu.Lock()
	e.lastFailure = time.Now().Add(-failureWindow - time.Second)
	e.rateLimitMu.Unlock()

	e.recordValidationFailure()

	if err := e.checkRateLimit(); err != nil {
		t.Errorf("expected no rate limit after window reset, got: %v", err)
	}
}

func TestRateLimitBlocksAfterMaxFailures(t *testing.T) {
	e := newTestExecutor(t)

	for range maxFailures {
		e.recordValidationFailure()
	}

	if err := e.checkRateLimit(); err == nil {
		t.Error("expected rate limit error after maxFailures")
	}
}

func TestRateLimitUnblocksAfterDuration(t *testing.T) {
	e := newTestExecutor(t)

	for range maxFailures {
		e.recordValidationFailure()
	}

	e.rateLimitMu.Lock()
	e.blockedUntil = time.Now().Add(-time.Second)
	e.rateLimitMu.Unlock()

	if err := e.checkRateLimit(); err != nil {
		t.Errorf("expected no rate limit after block duration, got: %v", err)
	}
}
