package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalIndexBlastRadius(t *testing.T) {
	dir := t.TempDir()

	// 1. Create a service file with a function
	serviceCode := `package service

func CalculateTax(amount float64) float64 {
	return amount * 0.11
}
`
	_ = os.WriteFile(filepath.Join(dir, "tax.go"), []byte(serviceCode), 0o644)

	// 2. Create caller files that reference CalculateTax
	caller1 := `package controller

import "service"

func HandleCheckout() {
	tax := service.CalculateTax(100.0)
	_ = tax
}
`
	_ = os.WriteFile(filepath.Join(dir, "checkout.go"), []byte(caller1), 0o644)

	caller2 := `package invoice

func GenerateInvoice() {
	t := CalculateTax(200.0)
	_ = t
}
`
	_ = os.WriteFile(filepath.Join(dir, "invoice.go"), []byte(caller2), 0o644)

	idx := BuildGlobalIndex(dir)
	if idx == nil {
		t.Fatal("failed to build global index")
	}

	report := idx.BlastRadius("CalculateTax")
	if report == nil {
		t.Fatal("expected non-nil blast radius report")
	}
	if report.Target != "CalculateTax" {
		t.Errorf("expected target 'CalculateTax', got %q", report.Target)
	}
	if report.CallersCount < 2 {
		t.Errorf("expected at least 2 callers, got %d", report.CallersCount)
	}
	formatted := report.Format()
	if !strings.Contains(formatted, "CalculateTax") || !strings.Contains(formatted, "checkout.go") {
		t.Errorf("expected formatted report to contain target and callers, got:\n%s", formatted)
	}
}
