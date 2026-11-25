package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
)

// Postgres is one run's databases on an existing server.
type Postgres struct {
	cfg    config.Postgres
	server string // URL without a database
	Name   string // per-run base name: <project>_localci_<pid>
}

// HostID identifies this machine in database names, so a run only ever sweeps databases
// that this machine's runs created: a PID means nothing on another host sharing the server.
func HostID() string {
	h, _ := os.Hostname()
	sum := sha256.Sum256([]byte(h))
	return hex.EncodeToString(sum[:])[:8]
}

// NewPostgres names this run's database <project>_localci_<hostid>_<pid>. LOCALCI_PG
// overrides the configured server URL.
func NewPostgres(project string, cfg config.Postgres) *Postgres {
	server := cfg.URL
	if v := os.Getenv("LOCALCI_PG"); v != "" {
		server = v
	}
	return &Postgres{cfg: cfg, server: server, Name: fmt.Sprintf("%s_localci_%s_%d", project, HostID(), os.Getpid())}
}

// URL returns the server URL with database db.
func (p *Postgres) URL(db string) (string, error) {
	u, err := url.Parse(p.server)
	if err != nil {
		return "", fmt.Errorf("postgres url: %w", err)
	}
	u.Path = "/" + db
	return u.String(), nil
}

func (p *Postgres) connect(ctx context.Context, db string) (*pgx.Conn, error) {
	u, err := p.URL(db)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return pgx.Connect(ctx, u)
}

func (p *Postgres) names() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{""}, p.cfg.Suffixes...), p.cfg.Create...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, p.Name+s)
		}
	}
	return out
}

// Setup drops leftovers (this PID's from a SIGKILLed run, and any whose PID is dead), then
// creates and connects to each database in Create. Test fixtures often skip when they
// cannot create their database, which would pass silently; this fails loudly instead.
func (p *Postgres) Setup(ctx context.Context, project string) error {
	admin, err := p.connect(ctx, "postgres")
	if err != nil {
		return fmt.Errorf("postgres: cannot reach %s: %w", p.server, err)
	}
	defer admin.Close(context.Background())

	if err := p.dropStale(ctx, admin, project); err != nil {
		return err
	}
	for _, n := range p.names() {
		if err := dropDB(ctx, admin, n); err != nil {
			return err
		}
	}
	for _, s := range p.cfg.Create {
		db := p.Name + s
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
			return fmt.Errorf("postgres: cannot create %s at %s (the role needs CREATEDB): %w", db, p.server, err)
		}
		c, err := p.connect(ctx, db)
		if err != nil {
			return fmt.Errorf("postgres: created %s but cannot connect to it: %w", db, err)
		}
		var one int
		err = c.QueryRow(ctx, "select 1").Scan(&one)
		c.Close(context.Background())
		if err != nil {
			return fmt.Errorf("postgres: query on %s failed: %w", db, err)
		}
	}
	return nil
}

// dropStale drops this host's <project>_localci_<hostid>_<pid>* databases whose PID is no
// longer running. Other hosts' databases are never touched.
func (p *Postgres) dropStale(ctx context.Context, admin *pgx.Conn, project string) error {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(project) + `_localci_` + HostID() + `_(\d+)`)
	rows, err := admin.Query(ctx, "select datname from pg_database")
	if err != nil {
		return fmt.Errorf("postgres: list databases: %w", err)
	}
	var stale []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		m := re.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		if pid != os.Getpid() && !PidAlive(pid) {
			stale = append(stale, n)
		}
	}
	rows.Close()
	for _, n := range stale {
		if err := dropDB(ctx, admin, n); err != nil {
			return err
		}
	}
	return nil
}

func dropDB(ctx context.Context, admin *pgx.Conn, db string) error {
	// WITH (FORCE) ends sessions a killed test run left open (Postgres 13+).
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("postgres: drop %s: %w", db, err)
	}
	return nil
}

// Teardown drops every database this run may have made. It uses its own context so it
// still runs after the run was cancelled.
func (p *Postgres) Teardown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := p.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	var first error
	for _, n := range p.names() {
		if err := dropDB(ctx, admin, n); err != nil && first == nil {
			first = err
		}
	}
	return first
}
