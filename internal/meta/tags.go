package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Tags (Nexus Pro's component tagging): a named label with free-form
// attributes, associated with components as the search derives them
// (component_tags). Repository deletes purge a repository's associations;
// a tag delete removes its own.

// Tag is one stored tag. Attributes is a JSON object.
type Tag struct {
	Name, Attributes string
	Created, Updated time.Time
}

// ComponentKey identifies a component: its repository and the group, name
// and version the search derives.
type ComponentKey struct{ Repo, Group, Name, Version string }

const tagCols = `name, attributes, created_at, updated_at`

func scanTag(row scanner) (Tag, error) {
	var t Tag
	var created, updated int64
	err := row.Scan(&t.Name, &t.Attributes, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, ErrNotFound
	}
	t.Created, t.Updated = fromMs(created), fromMs(updated)
	return t, err
}

// ListTags returns the tags ordered by name, at most limit of them after the
// name after ("" from the start).
func (d *DB) ListTags(ctx context.Context, after string, limit int) ([]Tag, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+tagCols+` FROM tags WHERE name > ? ORDER BY name LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTag returns one tag, or ErrNotFound.
func (d *DB) GetTag(ctx context.Context, name string) (Tag, error) {
	return scanTag(d.sql.QueryRowContext(ctx, `SELECT `+tagCols+` FROM tags WHERE name = ?`, name))
}

// CreateTag stores a new tag (its times now); ErrExists if the name is taken.
func (d *DB) CreateTag(ctx context.Context, name, attributes string) (Tag, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := d.Tx(ctx, func(tx *Tx) error {
		var one int
		switch err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE name = ?`, name).Scan(&one); {
		case err == nil:
			return ErrExists
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err := tx.q.ExecContext(ctx, `INSERT INTO tags(`+tagCols+`) VALUES(?,?,?,?)`, name, attributes, toMs(now), toMs(now))
		return err
	})
	return Tag{Name: name, Attributes: attributes, Created: now, Updated: now}, err
}

// UpdateTag replaces a tag's attributes; ErrNotFound if there is none.
func (d *DB) UpdateTag(ctx context.Context, name, attributes string) (Tag, error) {
	res, err := d.sql.ExecContext(ctx, `UPDATE tags SET attributes = ?, updated_at = ? WHERE name = ?`,
		attributes, toMs(time.Now()), name)
	if err := affected(res, err); err != nil {
		return Tag{}, err
	}
	return d.GetTag(ctx, name)
}

// DeleteTag removes a tag and its associations; ErrNotFound if there is none.
func (d *DB) DeleteTag(ctx context.Context, name string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		res, err := tx.q.ExecContext(ctx, `DELETE FROM tags WHERE name = ?`, name)
		if err := affected(res, err); err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `DELETE FROM component_tags WHERE tag = ?`, name)
		return err
	})
}

// AssociateTag labels the components with the tag (those labelled already
// stay so); ErrNotFound when the tag does not exist.
func (d *DB) AssociateTag(ctx context.Context, tag string, keys []ComponentKey) error {
	return d.Tx(ctx, func(tx *Tx) error {
		var one int
		switch err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE name = ?`, tag).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		}
		for _, k := range keys {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO component_tags(tag, repo, grp, name, version) VALUES(?,?,?,?,?)
ON CONFLICT(tag, repo, grp, name, version) DO NOTHING`, tag, k.Repo, k.Group, k.Name, k.Version); err != nil {
				return err
			}
		}
		_, err := tx.q.ExecContext(ctx, `UPDATE tags SET updated_at = ? WHERE name = ?`, toMs(time.Now()), tag)
		return err
	})
}

// DissociateTag removes the tag from the components; ErrNotFound when the
// tag does not exist.
func (d *DB) DissociateTag(ctx context.Context, tag string, keys []ComponentKey) error {
	return d.Tx(ctx, func(tx *Tx) error {
		var one int
		switch err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE name = ?`, tag).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		}
		for _, k := range keys {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM component_tags WHERE tag = ? AND repo = ? AND grp = ? AND name = ? AND version = ?`,
				tag, k.Repo, k.Group, k.Name, k.Version); err != nil {
				return err
			}
		}
		_, err := tx.q.ExecContext(ctx, `UPDATE tags SET updated_at = ? WHERE name = ?`, toMs(time.Now()), tag)
		return err
	})
}

// TagMembers returns the components associated with the tag (none for an
// unknown tag), ordered by key.
func (d *DB) TagMembers(ctx context.Context, tag string) ([]ComponentKey, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, grp, name, version FROM component_tags WHERE tag = ?
ORDER BY repo, grp, name, version`, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComponentKey
	for rows.Next() {
		var k ComponentKey
		if err := rows.Scan(&k.Repo, &k.Group, &k.Name, &k.Version); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ComponentTags returns the names of the tags associated with a component, sorted.
func (d *DB) ComponentTags(ctx context.Context, k ComponentKey) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT tag FROM component_tags WHERE repo = ? AND grp = ? AND name = ? AND version = ?
ORDER BY tag`, k.Repo, k.Group, k.Name, k.Version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DropComponentTags removes every association of a deleted component.
func (d *DB) DropComponentTags(ctx context.Context, k ComponentKey) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM component_tags WHERE repo = ? AND grp = ? AND name = ? AND version = ?`,
		k.Repo, k.Group, k.Name, k.Version)
	return err
}

// MoveComponentTags hands the associations of k to the component of the
// same group, name and version in repository to (a staging move).
func (d *DB) MoveComponentTags(ctx context.Context, k ComponentKey, to string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO component_tags(tag, repo, grp, name, version)
SELECT tag, ?, grp, name, version FROM component_tags WHERE repo = ? AND grp = ? AND name = ? AND version = ?
ON CONFLICT(tag, repo, grp, name, version) DO NOTHING`, to, k.Repo, k.Group, k.Name, k.Version); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `DELETE FROM component_tags WHERE repo = ? AND grp = ? AND name = ? AND version = ?`,
			k.Repo, k.Group, k.Name, k.Version)
		return err
	})
}

// BlobStoreMove is a repository's unfinished change of blob store: while
// its row exists, the repository reads blobs from Target, then Source, and
// writes them to Target (repo.Store).
type BlobStoreMove struct {
	Repo, Source, Target string
	Started              time.Time
}

// ListBlobStoreMoves returns the unfinished moves, by repository.
func (d *DB) ListBlobStoreMoves(ctx context.Context) ([]BlobStoreMove, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, source, target, started_at FROM blob_store_moves ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobStoreMove
	for rows.Next() {
		var m BlobStoreMove
		var started int64
		if err := rows.Scan(&m.Repo, &m.Source, &m.Target, &started); err != nil {
			return nil, err
		}
		m.Started = fromMs(started)
		out = append(out, m)
	}
	return out, rows.Err()
}

// PutBlobStoreMove records (or replaces) a repository's move.
func (d *DB) PutBlobStoreMove(ctx context.Context, m BlobStoreMove) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO blob_store_moves(repo, source, target, started_at) VALUES(?,?,?,?)
ON CONFLICT(repo) DO UPDATE SET source = excluded.source, target = excluded.target, started_at = excluded.started_at`,
		m.Repo, m.Source, m.Target, toMs(m.Started))
	return err
}

// DeleteBlobStoreMove removes a repository's move row, if any.
func (d *DB) DeleteBlobStoreMove(ctx context.Context, repo string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM blob_store_moves WHERE repo = ?`, repo)
	return err
}

// RepoBlob is one distinct blob the assets of a repository reference.
type RepoBlob struct {
	SHA256 string
	Size   int64
}

// RepoBlobs returns the distinct blobs repo's assets reference, by sha256.
func (d *DB) RepoBlobs(ctx context.Context, repo string) ([]RepoBlob, error) {
	return repoBlobs(ctx, d.sql, repo)
}

// RepoBlobs is DB.RepoBlobs inside the transaction.
func (t *Tx) RepoBlobs(ctx context.Context, repo string) ([]RepoBlob, error) {
	return repoBlobs(ctx, t.q, repo)
}

func repoBlobs(ctx context.Context, q querier, repo string) ([]RepoBlob, error) {
	rows, err := q.QueryContext(ctx, `SELECT blob_sha256, MAX(size) FROM assets WHERE repo = ? GROUP BY blob_sha256 ORDER BY blob_sha256`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoBlob
	for rows.Next() {
		var b RepoBlob
		if err := rows.Scan(&b.SHA256, &b.Size); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SwitchBlobStore ends a repository's move in one transaction: check runs
// first (an error from it changes nothing), then the stored definition's
// blobStore becomes store and the move row goes. ErrNotFound when the
// repository does not exist.
func (d *DB) SwitchBlobStore(ctx context.Context, repo, store string, check func(*Tx) error) error {
	return d.Tx(ctx, func(tx *Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		var stored string
		err := tx.q.QueryRowContext(ctx, `SELECT config FROM repositories WHERE name = ?`, repo).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// The definition is patched as JSON: its sealed secrets stay as stored.
		var def map[string]json.RawMessage
		if err := json.Unmarshal([]byte(stored), &def); err != nil {
			return fmt.Errorf("repository %q: decode stored definition: %w", repo, err)
		}
		def["blobStore"], _ = json.Marshal(store)
		js, err := json.Marshal(def)
		if err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE repositories SET config = ?, updated_at = ? WHERE name = ?`,
			string(js), toMs(time.Now()), repo); err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `DELETE FROM blob_store_moves WHERE repo = ?`, repo)
		return err
	})
}
