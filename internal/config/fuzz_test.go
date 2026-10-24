package config

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func FuzzParseProviderBaseURL(f *testing.F) {
	f.Add("https://example.com/")
	f.Add("http://127.0.0.1:8080")
	f.Add("ftp://example.com")
	f.Add("example.com")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := parseProviderBaseURL(raw)
		parsed, parseErr := url.Parse(strings.TrimSpace(raw))
		wantErr := parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == ""
		if (err != nil) != wantErr {
			t.Fatalf("parseProviderBaseURL(%q) error = %v, want error %v", raw, err, wantErr)
		}
		if err != nil {
			return
		}
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		if want := parsed.String(); got != want {
			t.Fatalf("parseProviderBaseURL(%q) = %q, want %q", raw, got, want)
		}
	})
}

func FuzzParseGuestUserAuthTags(f *testing.F) {
	f.Add("alice@example.com=tag:dev")
	f.Add("alice=tag:dev,bob=tag:ops")
	f.Add("alice=dev")
	f.Add("alice=tag:")
	f.Fuzz(func(t *testing.T, raw string) {
		tags, err := parseGuestUserAuthTags(raw)
		want, wantErr := expectedGuestUserAuthTags(raw)
		if (err != nil) != wantErr {
			t.Fatalf("parseGuestUserAuthTags(%q) error = %v, want error %v", raw, err, wantErr)
		}
		if err != nil {
			return
		}
		if !reflect.DeepEqual(tags, want) {
			t.Fatalf("parseGuestUserAuthTags(%q) = %#v, want %#v", raw, tags, want)
		}
	})
}

func expectedGuestUserAuthTags(raw string) (map[string]string, bool) {
	var entries []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, false
	}
	want := make(map[string]string, len(entries))
	for _, entry := range entries {
		login, tag, found := strings.Cut(entry, "=")
		login = strings.ToLower(strings.TrimSpace(login))
		tag = strings.ToLower(strings.TrimSpace(tag))
		if !found || login == "" || strings.Contains(tag, "=") || !strings.HasPrefix(tag, "tag:") || len(tag) == len("tag:") {
			return nil, true
		}
		if _, exists := want[login]; exists {
			return nil, true
		}
		want[login] = tag
	}
	return want, false
}

func FuzzValidateMachineShape(f *testing.F) {
	f.Add(int64(1), int64(128))
	f.Add(int64(2), int64(512))
	f.Add(int64(3), int64(128))
	f.Add(int64(0), int64(0))
	f.Fuzz(func(t *testing.T, vcpus, memoryMiB int64) {
		err := ValidateMachineShape(vcpus, memoryMiB)
		wantErr := vcpus < 1 || vcpus > MaxVCPUCount || (vcpus != 1 && vcpus%2 != 0) || memoryMiB < MinMemoryMiB
		if (err != nil) != wantErr {
			t.Fatalf("ValidateMachineShape(%d, %d) error = %v, want error %v", vcpus, memoryMiB, err, wantErr)
		}
	})
}
