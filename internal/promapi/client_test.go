package promapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"result":[
			{"metric":{"pod":"w-0"},"value":[1700000000,"97.5"]},
			{"metric":{"pod":"w-1"},"value":[1700000000,"0"]}]}}`)
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL).Query(context.Background(), "up")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Labels["pod"] != "w-0" || got[0].Value != 97.5 || got[1].Value != 0 {
		t.Errorf("unexpected samples: %+v", got)
	}
}
