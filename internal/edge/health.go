package edge

import (
	"time"

	"jellyfinner/internal/model"
)

// HealthWindow is how recently an agent must have reported for its node to be
// used.
const HealthWindow = time.Minute

// Servable is the health rule: the orchestrator's probe passed and the agent
// reported recently.
func Servable(st model.NodeStatus, now time.Time) bool {
	return st.Healthy && now.Sub(time.Unix(st.LastSeen, 0)) < HealthWindow
}
