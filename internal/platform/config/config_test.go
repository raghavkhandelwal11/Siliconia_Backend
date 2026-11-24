package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("APP_MODE", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Mode != ModeLocal || c.HTTPAddr != ":8080" || !c.FeatureTestInbox {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestProductionRefusesSimulators(t *testing.T) {
	t.Setenv("APP_MODE", "production")
	t.Setenv("PROVIDER_SMS", "sim")
	if _, err := Load(); err == nil {
		t.Fatal("expected production mode with sim SMS to be refused")
	}
}

func TestUnknownMode(t *testing.T) {
	t.Setenv("APP_MODE", "staging")
	if _, err := Load(); err == nil {
		t.Fatal("expected unknown mode to be refused")
	}
}
