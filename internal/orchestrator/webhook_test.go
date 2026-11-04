package orchestrator

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The template is pasted into Jellyfin, so nothing else connects it to the code.
func TestWebhookTemplateSendsEveryField(t *testing.T) {
	data, err := os.ReadFile("../../deploy/jellyfin-webhook.json")
	if err != nil {
		t.Fatal(err)
	}
	var template map[string]string
	if err := json.Unmarshal(data, &template); err != nil {
		t.Fatal(err)
	}
	event := reflect.TypeFor[PlaybackEvent]()
	for i := range event.NumField() {
		if key := event.Field(i).Tag.Get("json"); template[key] == "" {
			t.Errorf("the template does not send %s", key)
		}
	}
}
