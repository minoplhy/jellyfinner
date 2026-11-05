package jellyfin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestUsersSortedWithTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jf/Users" || r.Header.Get("Authorization") != `MediaBrowser Token="key"` {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`[{"Name":"bob"},{"Name":"Alice"},{"Name":""},{"Name":"carol"}]`))
	}))
	defer srv.Close()

	got, err := New(srv.URL+"/jf", "key").Users(context.Background())
	if want := []string{"Alice", "bob", "carol"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Users = %v, %v", got, err)
	}
	if _, err := New(srv.URL+"/jf", "wrong").Users(context.Background()); err == nil {
		t.Error("a refused token must fail")
	}
	if _, err := New(srv.URL+"/jf", "").Users(context.Background()); err == nil {
		t.Error("no token must fail before asking")
	}
}
