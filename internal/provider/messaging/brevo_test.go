package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrevoSendsTransactionalEmail(t *testing.T) {
	var got map[string]any
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("api-key")
		if r.URL.Path != "/v3/smtp/email" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"messageId":"<abc@brevo>"}`))
	}))
	defer srv.Close()
	if _, err := NewBrevo(BrevoConfig{APIKey: "k"}); err == nil {
		t.Fatal("a sender address is required")
	}
	b, err := NewBrevo(BrevoConfig{APIKey: "k", From: "orders@siliconics.test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send(context.Background(), Message{Channel: SMS, To: "+919876500000", Body: "x"}); err == nil {
		t.Fatal("SMS must be refused")
	}
	ref, err := b.Send(context.Background(), Message{Channel: Email, To: "asha@example.com", Template: "order_confirmed", Subject: "Order confirmed", Body: "Hello"})
	if err != nil || ref != "<abc@brevo>" || key != "k" {
		t.Fatalf("send: %q %v key=%q", ref, err, key)
	}
	to := got["to"].([]any)[0].(map[string]any)["email"]
	if got["subject"] != "Order confirmed" || to != "asha@example.com" || got["textContent"] != "Hello" {
		t.Fatalf("body: %v", got)
	}
}
