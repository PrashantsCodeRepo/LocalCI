package provider

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
)

// Set LOCALCI_TEST_PG to a server URL whose role may create databases, e.g.
// postgresql://localhost:5432, to run these.
func testServer(t *testing.T) string {
	u := os.Getenv("LOCALCI_TEST_PG")
	if u == "" {
		t.Skip("LOCALCI_TEST_PG not set")
	}
	return u
}

func dbExists(t *testing.T, p *Postgres, name string) bool {
	t.Helper()
	c, err := p.connect(context.Background(), "postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var n int
	if err := c.QueryRow(context.Background(), "select count(*) from pg_database where datname=$1", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestPostgresSetupAndTeardown(t *testing.T) {
	server := testServer(t)
	p := NewPostgres("localcitest", config.Postgres{URL: server, Suffixes: []string{"_test", "_eval"}, Create: []string{"_test"}})
	ctx := context.Background()

	// A database the code under test derived (like <name>_eval) must also be dropped.
	if err := p.Setup(ctx, "localcitest"); err != nil {
		t.Fatal(err)
	}
	if !dbExists(t, p, p.Name+"_test") {
		t.Fatal("Setup did not create _test")
	}
	admin, _ := p.connect(ctx, "postgres")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{p.Name + "_eval"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)

	if err := p.Teardown(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"_test", "_eval"} {
		if dbExists(t, p, p.Name+s) {
			t.Errorf("%s%s survived Teardown", p.Name, s)
		}
	}
}

func TestPostgresDropsDatabasesOfDeadRuns(t *testing.T) {
	server := testServer(t)
	p := NewPostgres("localcitest", config.Postgres{URL: server, Create: []string{""}})
	ctx := context.Background()
	stale := "localcitest_localci_" + strconv.Itoa(deadPID(t)) + "_test"
	admin, err := p.connect(ctx, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{stale}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	if err := p.Setup(ctx, "localcitest"); err != nil {
		t.Fatal(err)
	}
	defer p.Teardown()
	if dbExists(t, p, stale) {
		t.Errorf("stale %s survived Setup", stale)
	}
}

func TestPostgresSetupFailsLoudlyWhenServerUnreachable(t *testing.T) {
	t.Setenv("LOCALCI_PG", "")
	p := NewPostgres("localcitest", config.Postgres{URL: "postgresql://localhost:1", Create: []string{""}})
	if err := p.Setup(context.Background(), "localcitest"); err == nil {
		t.Fatal("Setup succeeded against a closed port")
	}
}
