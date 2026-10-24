package service

import (
	"math"
	"path"
	"strconv"
	"strings"
	"testing"
)

func FuzzParseCommandRequest(f *testing.F) {
	f.Add("list")
	f.Add("--json list")
	f.Add("--json")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		args := strings.Fields(raw)
		request, err := parseCommandRequest(args)
		wantErr := len(args) == 1 && args[0] == "--json"
		if (err != nil) != wantErr {
			t.Fatalf("parseCommandRequest(%#v) error = %v, want error %v", args, err, wantErr)
		}
		if err != nil {
			return
		}
		want := args
		jsonOutput := len(args) > 0 && args[0] == "--json"
		if jsonOutput {
			want = args[1:]
		}
		if len(request.args) != len(want) {
			t.Fatalf("args = %#v, want %#v", request.args, want)
		}
		for i := range want {
			if request.args[i] != want[i] {
				t.Fatalf("args[%d] = %q, want %q", i, request.args[i], want[i])
			}
		}
		if (request.format == outputFormatJSON) != jsonOutput {
			t.Fatalf("format = %v with args %#v", request.format, args)
		}
	})
}

func FuzzParseImportOptions(f *testing.F) {
	f.Add("")
	f.Add("--fixed-memory")
	f.Add("--pool pool-1")
	f.Add("--pool")
	f.Add("--fixed-memory --pool pool-1")
	f.Fuzz(func(t *testing.T, raw string) {
		args := append([]string{"import"}, strings.Fields(raw)...)
		opts, err := parseImportOptions(args)
		wantFixed, wantPool, wantErr := expectedImportOptions(args)
		if (err != nil) != wantErr {
			t.Fatalf("parseImportOptions(%#v) error = %v, want error %v", args, err, wantErr)
		}
		if err != nil {
			return
		}
		if opts.FixedMemory != wantFixed || opts.MemoryPoolName != wantPool {
			t.Fatalf("parseImportOptions(%#v) = %+v, want fixed=%v pool=%q", args, opts, wantFixed, wantPool)
		}
	})
}

func expectedImportOptions(args []string) (fixed bool, pool string, wantErr bool) {
	seen := false
	for i := 1; i < len(args); i++ {
		if seen {
			return false, "", true
		}
		seen = true
		switch args[i] {
		case "--fixed-memory":
			fixed = true
		case "--pool":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--") {
				return false, "", true
			}
			i++
			pool = args[i]
		default:
			return false, "", true
		}
	}
	return fixed, pool, false
}

func FuzzParseSize(f *testing.F) {
	f.Add(int64(1), "", false)
	f.Add(int64(512), "m", false)
	f.Add(int64(1), "GiB", true)
	f.Add(int64(0), "", false)
	f.Add(int64(-1), "", false)
	f.Add(int64(1), "x", false)
	f.Add(int64(math.MaxInt64), "b", false)
	f.Add(int64(math.MaxInt64/(1<<40)+1), "t", false)
	f.Add(int64(16777217), "t", false)
	f.Add(int64(1), "MiB", true)
	f.Fuzz(func(t *testing.T, number int64, unit string, mibDefault bool) {
		if unit != "" && unit[0] >= '0' && unit[0] <= '9' {
			return // The digit would extend the number prefix.
		}
		defaultUnit := int64(1)
		if mibDefault {
			defaultUnit = 1 << 20
		}
		raw := strconv.FormatInt(number, 10) + unit
		got, err := parseSize(raw, defaultUnit)
		want, wantOK := expectedSize(number, unit, defaultUnit)
		if (err == nil) != wantOK {
			t.Fatalf("parseSize(%q, %d) = %d, %v; want success %v", raw, defaultUnit, got, err, wantOK)
		}
		if err == nil && got != want {
			t.Fatalf("parseSize(%q, %d) = %d, want %d", raw, defaultUnit, got, want)
		}
	})
}

func expectedSize(number int64, unit string, defaultUnit int64) (int64, bool) {
	if number <= 0 {
		return 0, false
	}
	multiplier, ok := map[string]int64{
		"":    defaultUnit,
		"b":   1,
		"k":   1 << 10,
		"kb":  1 << 10,
		"kib": 1 << 10,
		"m":   1 << 20,
		"mb":  1 << 20,
		"mib": 1 << 20,
		"g":   1 << 30,
		"gb":  1 << 30,
		"gib": 1 << 30,
		"t":   1 << 40,
		"tb":  1 << 40,
		"tib": 1 << 40,
	}[strings.ToLower(strings.TrimSpace(unit))]
	if !ok {
		return 0, false
	}
	if number > math.MaxInt64/multiplier {
		return 0, false
	}
	return number * multiplier, true
}

func FuzzIntegrationGatewayPath(f *testing.F) {
	f.Add("/integrations/demo")
	f.Add("/integrations/demo/v1/items")
	f.Add("/integrations//demo")
	f.Add("/../integrations/demo")
	f.Add("/integrations/%2e%2e/demo")
	f.Fuzz(func(t *testing.T, raw string) {
		trimmed := strings.TrimSpace(raw)
		wantCleaned := "/"
		if trimmed != "" {
			wantCleaned = path.Clean("/" + strings.TrimLeft(trimmed, "/"))
			if strings.HasSuffix(trimmed, "/") && wantCleaned != "/" && !strings.HasSuffix(wantCleaned, "/") {
				wantCleaned += "/"
			}
		}
		if cleaned := cleanIntegrationGatewayPath(raw); cleaned != wantCleaned {
			t.Fatalf("cleanIntegrationGatewayPath(%q) = %q, want %q", raw, cleaned, wantCleaned)
		}

		wantOK := strings.HasPrefix(wantCleaned, "/integrations/")
		var wantName, wantUpstream string
		if wantOK {
			rest := strings.TrimPrefix(wantCleaned, "/integrations/")
			var remainder string
			wantName, remainder, _ = strings.Cut(rest, "/")
			wantName = strings.TrimSpace(wantName)
			if wantName == "" {
				wantOK = false
			} else if remainder == "" {
				wantUpstream = "/"
			} else {
				wantUpstream = "/" + remainder
			}
		}
		name, upstreamPath, ok := parseIntegrationGatewayPath(raw)
		if ok != wantOK || name != wantName || upstreamPath != wantUpstream {
			t.Fatalf("parseIntegrationGatewayPath(%q) = %q, %q, %v; want %q, %q, %v", raw, name, upstreamPath, ok, wantName, wantUpstream, wantOK)
		}
	})
}
