package migrate_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/tern/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var versionTable string = "schema_version_non_default"

func connectConn(t testing.TB) *pgx.Conn {
	prepareDatabase(t)

	conn, err := pgx.Connect(context.Background(), os.Getenv("MIGRATE_TEST_CONN_STRING"))
	assert.NoError(t, err)

	var currentUser string
	err = conn.QueryRow(context.Background(), "select current_user").Scan(&currentUser)
	assert.NoError(t, err)
	_, err = conn.Exec(context.Background(), fmt.Sprintf("drop schema if exists %s cascade", currentUser))
	assert.NoError(t, err)

	return conn
}

func prepareDatabase(t testing.TB) {
	connString, _ := os.LookupEnv("MIGRATE_TEST_DATABASE")
	require.NotEqualf(t, "", connString, "MIGRATE_TEST_DATABASE must be set")

	cmd := exec.Command("dropdb", connString)
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "dropdb failed with output: %s", string(output))

	cmd = exec.Command("createdb", connString)
	output, err = cmd.CombinedOutput()
	require.NoErrorf(t, err, "createdb failed with output: %s", string(output))
}

func currentVersion(t testing.TB, conn *pgx.Conn) int32 {
	var n int32
	err := conn.QueryRow(context.Background(), "select version from "+versionTable).Scan(&n)
	assert.NoError(t, err)
	return n
}

func mustExec(t testing.TB, conn *pgx.Conn, sql string, arguments ...interface{}) pgconn.CommandTag {
	commandTag, err := conn.Exec(context.Background(), sql, arguments...)
	assert.NoError(t, err)
	return commandTag
}

func tableExists(t testing.TB, conn *pgx.Conn, tableName string) bool {
	var exists bool
	err := conn.QueryRow(
		context.Background(),
		"select exists(select 1 from information_schema.tables where table_catalog=current_database() and table_name=$1)",
		tableName,
	).Scan(&exists)
	assert.NoError(t, err)
	return exists
}

func createEmptyMigrator(t testing.TB, conn *pgx.Conn) *migrate.Migrator {
	var err error
	m, err := migrate.NewMigrator(context.Background(), conn, versionTable)
	assert.NoError(t, err)
	return m
}

func createSampleMigrator(t testing.TB, conn *pgx.Conn) *migrate.Migrator {
	m := createEmptyMigrator(t, conn)
	m.AppendMigration("Create t1", "create table t1(id serial);")
	m.AppendMigration("Create t2", "create table t2(id serial);")
	m.AppendMigration("Create t3", "create table t3(id serial);")
	return m
}

func TestNewMigrator(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())

	// Initial run
	m, err := migrate.NewMigrator(context.Background(), conn, versionTable)
	assert.NoError(t, err)

	// Creates version table
	schemaVersionExists := tableExists(t, conn, versionTable)
	require.True(t, schemaVersionExists)

	// Succeeds when version table is already created
	m, err = migrate.NewMigrator(context.Background(), conn, versionTable)
	assert.NoError(t, err)

	initialVersion, err := m.GetCurrentVersion(context.Background())
	assert.NoError(t, err)
	require.EqualValues(t, 0, initialVersion)
}

func TestAppendMigration(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createEmptyMigrator(t, conn)

	name := "Create t"
	upSQL := "create t..."
	m.AppendMigration(name, upSQL)

	assert.Len(t, m.Migrations, 1)
	assert.Equal(t, m.Migrations[0].Name, name)
	assert.Equal(t, m.Migrations[0].SQL, upSQL)
}

func TestLoadMigrationsMissingDirectory(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createEmptyMigrator(t, conn)

	err := m.LoadMigrations("testdata/missing")
	require.EqualError(t, err, "open testdata/missing: no such file or directory")
}

func TestLoadMigrationsEmptyDirectory(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createEmptyMigrator(t, conn)

	err := m.LoadMigrations("testdata/empty")
	require.EqualError(t, err, "No migrations found at testdata/empty")
}

func TestFindMigrationsWithGaps(t *testing.T) {
	_, err := migrate.FindMigrations("testdata/gap")
	require.EqualError(t, err, "Missing migration 2")
}

func TestFindMigrationsWithDuplicate(t *testing.T) {
	_, err := migrate.FindMigrations("testdata/duplicate")
	require.EqualError(t, err, "Duplicate migration 2")
}

func TestLoadMigrations(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createEmptyMigrator(t, conn)

	m.Data = map[string]interface{}{"prefix": "foo"}
	err := m.LoadMigrations("testdata/sample")
	require.NoError(t, err)
	require.Len(t, m.Migrations, 6)

	assert.Equal(t, "001_create_t1.sql", m.Migrations[0].Name)
	assert.Equal(t, `create table t1(
  id serial primary key
);`, m.Migrations[0].SQL)

	assert.Equal(t, "002_create_t2.sql", m.Migrations[1].Name)
	assert.Equal(t, `create table t2(
  id serial primary key
);`, m.Migrations[1].SQL)

	assert.Equal(t, "003_irreversible.sql", m.Migrations[2].Name)
	assert.Equal(t, "drop table t2;", m.Migrations[2].SQL)

	assert.Equal(t, "004_data_interpolation.sql", m.Migrations[3].Name)
	assert.Equal(t, "create table foo_bar(id serial primary key);", m.Migrations[3].SQL)

	assert.Equal(t, "006_sprig.sql", m.Migrations[5].Name)
	assert.Equal(t, "create table baz_42(id serial primary key);", m.Migrations[5].SQL)
}

func TestLoadMigrationsNoForward(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())

	m, err := migrate.NewMigrator(context.Background(), conn, versionTable)
	assert.NoError(t, err)

	m.Data = map[string]interface{}{"prefix": "foo"}
	err = m.LoadMigrations("testdata/noforward")
	require.Equal(t, migrate.ErrNoFwMigration, err)
}

func TestMigrate(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createSampleMigrator(t, conn)

	err := m.Migrate(context.Background())
	assert.NoError(t, err)
	currentVersion := currentVersion(t, conn)
	assert.EqualValues(t, 3, currentVersion)
}

func TestMigrateToLifeCycle(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createSampleMigrator(t, conn)

	var onStartCallCount int
	m.OnStart = func(_ int32, _, _ string) {
		onStartCallCount++
	}

	// Migrate from 0 up to 1
	err := m.MigrateTo(context.Background(), 1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, currentVersion(t, conn))
	assert.True(t, tableExists(t, conn, "t1"))
	assert.False(t, tableExists(t, conn, "t2"))
	assert.False(t, tableExists(t, conn, "t3"))
	assert.EqualValues(t, 1, onStartCallCount)

	// Migrate from 1 up to 3
	err = m.MigrateTo(context.Background(), 3)
	require.NoError(t, err)
	assert.EqualValues(t, 3, currentVersion(t, conn))
	assert.True(t, tableExists(t, conn, "t1"))
	assert.True(t, tableExists(t, conn, "t2"))
	assert.True(t, tableExists(t, conn, "t3"))
	assert.EqualValues(t, 3, onStartCallCount)

	// Migrate from 3 to 3 is no-op
	err = m.MigrateTo(context.Background(), 3)
	require.NoError(t, err)
	assert.EqualValues(t, 3, currentVersion(t, conn))
	assert.True(t, tableExists(t, conn, "t1"))
	assert.True(t, tableExists(t, conn, "t2"))
	assert.True(t, tableExists(t, conn, "t3"))
	assert.EqualValues(t, 3, onStartCallCount)
}

func TestMigrateToBoundaries(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createSampleMigrator(t, conn)

	// Migrate to -1 is error
	err := m.MigrateTo(context.Background(), -1)
	require.EqualError(t, err, "destination version -1 is outside the valid versions of 0 to 3")

	// Migrate past end is error
	err = m.MigrateTo(context.Background(), int32(len(m.Migrations))+1)
	require.EqualError(t, err, "destination version 4 is outside the valid versions of 0 to 3")

	// When schema version is past last version
	mustExec(t, conn, "update "+versionTable+" set version=4")
	err = m.MigrateTo(context.Background(), int32(3))
	require.EqualError(t, err, "current version 4 is greater than last version of 3")
}

func TestMigrateToDisableTx(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())

	m, err := migrate.NewMigratorEx(context.Background(), conn, versionTable, &migrate.MigratorOptions{DisableTx: true})
	assert.NoError(t, err)
	m.AppendMigration("Create t1", "create table t1(id serial);")
	m.AppendMigration("Create t2", "create table t2(id serial);")
	m.AppendMigration("Create t3", "create table t3(id serial);")

	tx, err := conn.Begin(context.Background())
	assert.NoError(t, err)

	err = m.MigrateTo(context.Background(), 3)
	assert.NoError(t, err)
	require.EqualValues(t, 3, currentVersion(t, conn))
	require.True(t, tableExists(t, conn, "t1"))
	require.True(t, tableExists(t, conn, "t2"))
	require.True(t, tableExists(t, conn, "t3"))

	err = tx.Rollback(context.Background())
	assert.NoError(t, err)
	require.EqualValues(t, 0, currentVersion(t, conn))
	require.False(t, tableExists(t, conn, "t1"))
	require.False(t, tableExists(t, conn, "t2"))
	require.False(t, tableExists(t, conn, "t3"))
}

// // https://github.com/jackc/tern/issues/18
func TestNotCreatingVersionTableIfAlreadyVisibleInSearchPath(t *testing.T) {
	conn := connectConn(t)
	defer conn.Close(context.Background())
	m := createSampleMigrator(t, conn)

	err := m.Migrate(context.Background())
	assert.NoError(t, err)
	currentVersion := currentVersion(t, conn)
	require.EqualValues(t, 3, currentVersion)

	var currentUser string
	err = conn.QueryRow(context.Background(), "select current_user").Scan(&currentUser)
	assert.NoError(t, err)
	_, err = conn.Exec(context.Background(), fmt.Sprintf("create schema %s", currentUser))
	assert.NoError(t, err)

	m = createSampleMigrator(t, conn)
	mCurrentVersion, err := m.GetCurrentVersion(context.Background())
	assert.NoError(t, err)
	require.EqualValues(t, 3, mCurrentVersion)
}

func Example_OnStartMigrationProgressLogging() {
	conn, err := pgx.Connect(context.Background(), os.Getenv("MIGRATE_TEST_CONN_STRING"))
	if err != nil {
		fmt.Printf("Unable to establish connection: %v", err)
		return
	}

	// Clear any previous runs
	if _, err = conn.Exec(context.Background(), "drop table if exists schema_version"); err != nil {
		fmt.Printf("Unable to drop schema_version table: %v", err)
		return
	}

	var m *migrate.Migrator
	m, err = migrate.NewMigrator(context.Background(), conn, "schema_version")
	if err != nil {
		fmt.Printf("Unable to create migrator: %v", err)
		return
	}

	m.OnStart = func(_ int32, name, _ string) {
		fmt.Printf("Migrating: %s", name)
	}

	m.AppendMigration("create a table", "create temporary table foo(id serial primary key)")

	if err = m.Migrate(context.Background()); err != nil {
		fmt.Printf("Unexpected failure migrating: %v", err)
		return
	}
	// Output:
	// Migrating: create a table
}
