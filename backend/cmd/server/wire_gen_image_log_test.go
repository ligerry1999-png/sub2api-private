package main

import (
	"os"
	"strings"
	"testing"
)

func TestGeneratedWireUsesOpenAIGatewayStartupProvider(t *testing.T) {
	generated, err := os.ReadFile("wire_gen.go")
	if err != nil {
		t.Fatalf("read wire_gen.go: %v", err)
	}

	source := string(generated)
	if !strings.Contains(source, "service.ProvideOpenAIGatewayService(") {
		t.Fatal("wire_gen.go bypasses the OpenAI gateway startup provider; image logging can be silently disconnected")
	}
	if strings.Contains(source, "service.NewOpenAIGatewayService(") {
		t.Fatal("wire_gen.go constructs OpenAIGatewayService directly instead of enforcing required startup dependencies")
	}
}
