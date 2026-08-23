package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/search"
)

func TestCodeSliceTool(t *testing.T) {
	dir := t.TempDir()

	orderServiceCode := `package service

type OrderService struct {
	Repo OrderRepo
}

func (s *OrderService) ProcessPayment(orderID string, amount float64) error {
	order := s.Repo.FindByID(orderID)
	return s.Repo.Save(order)
}
`
	controllerCode := `package controller

import "service"

func HandleCheckout(s *service.OrderService) {
	s.ProcessPayment("order_123", 99.50)
}
`
	repoCode := `package service

type OrderRepo struct{}

func (r *OrderRepo) FindByID(id string) string {
	return id
}

func (r *OrderRepo) Save(order string) error {
	return nil
}
`

	if err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(orderServiceCode), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "controller.go"), []byte(controllerCode), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo.go"), []byte(repoCode), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := search.BuildGlobalIndex(dir)
	sliceTool := &CodeSliceTool{Index: idx}

	// 1. Slice ProcessPayment
	res, err := sliceTool.Execute(context.Background(), `{"name":"ProcessPayment"}`)
	if err != nil {
		t.Fatalf("code_slice failed: %v", err)
	}

	if !strings.Contains(res, "ProcessPayment") {
		t.Errorf("expected ProcessPayment in output, got:\n%s", res)
	}
	if !strings.Contains(res, "Inbound Callers") || !strings.Contains(res, "controller.go") {
		t.Errorf("expected inbound caller in controller.go, got:\n%s", res)
	}
	if !strings.Contains(res, "Outbound Dependencies") || !strings.Contains(res, "FindByID") {
		t.Errorf("expected outbound dependencies (FindByID / Save), got:\n%s", res)
	}
}
