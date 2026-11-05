package sqlite

import (
	"fmt"
	"time"

	"jellyfinner/internal/model"
)

const cloudColumns = `provider, id, name, node, type, location, resources, state, created, first_seen,
	idle_since, delete_at, deleted, alerted, hourly_price, currency`

func cloudArgs(c *model.CloudServer) []any {
	return []any{c.Provider, c.ID, c.Name, c.Node, c.Type, c.Location, c.Resources, c.State, c.Created, c.FirstSeen,
		c.IdleSince, c.DeleteAt, c.Deleted, c.Alerted, c.HourlyPrice, c.Currency}
}

// AddCloudServer stores a new server.
func (s *Store) AddCloudServer(c *model.CloudServer) error {
	_, err := s.db.Exec(`INSERT INTO cloud_servers (`+cloudColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cloudArgs(c)...)
	return err
}

// UpdateCloudServer saves every field of one server.
func (s *Store) UpdateCloudServer(c *model.CloudServer) error {
	res, err := s.db.Exec(`UPDATE cloud_servers SET name = ?, node = ?, type = ?, location = ?, resources = ?, state = ?,
		created = ?, first_seen = ?, idle_since = ?, delete_at = ?, deleted = ?, alerted = ?, hourly_price = ?, currency = ?
		WHERE provider = ? AND id = ?`,
		c.Name, c.Node, c.Type, c.Location, c.Resources, c.State, c.Created, c.FirstSeen, c.IdleSince, c.DeleteAt,
		c.Deleted, c.Alerted, c.HourlyPrice, c.Currency, c.Provider, c.ID)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return fmt.Errorf("cloud server %s/%s: %w", c.Provider, c.ID, model.ErrNotFound)
	}
	return nil
}

// CloudServers returns live servers and those deleted at or after since.
func (s *Store) CloudServers(since time.Time) ([]*model.CloudServer, error) {
	rows, err := s.db.Query(`SELECT `+cloudColumns+` FROM cloud_servers
		WHERE state != ? OR deleted >= ? ORDER BY created, provider, id`, model.CloudDeleted, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.CloudServer
	for rows.Next() {
		var c model.CloudServer
		if err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Node, &c.Type, &c.Location, &c.Resources, &c.State, &c.Created,
			&c.FirstSeen, &c.IdleSince, &c.DeleteAt, &c.Deleted, &c.Alerted, &c.HourlyPrice, &c.Currency); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
