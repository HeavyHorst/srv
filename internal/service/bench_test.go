package service

import "testing"

func BenchmarkParseCommandRequest(b *testing.B) {
	args := []string{"--json", "list"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseCommandRequest(args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseSize(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseSize("512MiB", 1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIntegrationGatewayPath(b *testing.B) {
	raw := "/integrations/demo/v1/items"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := parseIntegrationGatewayPath(raw); !ok {
			b.Fatal("path was rejected")
		}
	}
}
