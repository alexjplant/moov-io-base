package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/moov-io/base"
	"github.com/moov-io/base/docker"

	kitprom "github.com/go-kit/kit/metrics/prometheus"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/ory/dockertest/v3"
	dc "github.com/ory/dockertest/v3/docker"
	stdprom "github.com/prometheus/client_golang/prometheus"

	"github.com/moov-io/base/log"
)

var (
	mysqlConnections = kitprom.NewGaugeFrom(stdprom.GaugeOpts{
		Name: "mysql_connections",
		Help: "How many MySQL connections and what status they're in.",
	}, []string{"state"})

	// mySQLErrDuplicateKey is the error code for duplicate entries
	// https://dev.mysql.com/doc/refman/8.0/en/server-error-reference.html#error_er_dup_entry
	mySQLErrDuplicateKey uint16 = 1062

	maxActiveMySQLConnections = func() int {
		if v := os.Getenv("MYSQL_MAX_CONNECTIONS"); v != "" {
			if n, _ := strconv.ParseInt(v, 10, 32); n > 0 {
				return int(n)
			}
		}
		return 16
	}()
)

type discardLogger struct{}

func (l discardLogger) Print(v ...interface{}) {}

func init() {
	gomysql.SetLogger(discardLogger{})
}

type mysql struct {
	dsn    string
	logger log.Logger

	connections *kitprom.Gauge
}

func (my *mysql) Connect(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("mysql", my.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxActiveMySQLConnections)

	// Check out DB is up and working
	if err := db.Ping(); err != nil {
		return nil, err
	}

	// Setup metrics after the database is setup
	go func() {
		t := time.NewTicker(1 * time.Minute)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				stats := db.Stats()
				my.connections.With("state", "idle").Set(float64(stats.Idle))
				my.connections.With("state", "inuse").Set(float64(stats.InUse))
				my.connections.With("state", "open").Set(float64(stats.OpenConnections))
			}
		}
	}()

	return db, nil
}

func mysqlConnection(logger log.Logger, user, pass string, address string, database string) *mysql {
	timeout := "30s"
	if v := os.Getenv("MYSQL_TIMEOUT"); v != "" {
		timeout = v
	}
	params := fmt.Sprintf("timeout=%s&charset=utf8mb4&parseTime=true&sql_mode=ALLOW_INVALID_DATES", timeout)
	dsn := fmt.Sprintf("%s:%s@%s/%s?%s", user, pass, address, database, params)
	return &mysql{
		dsn:         dsn,
		logger:      logger,
		connections: mysqlConnections,
	}
}

// TestMySQLDB is a wrapper around sql.DB for MySQL connections designed for tests to provide
// a clean database for each testcase.  Callers should cleanup with Close() when finished.
type TestMySQLDB struct {
	*sql.DB
	name     string
	shutdown func() // context shutdown func
	t        *testing.T
}

func (r *TestMySQLDB) Close() error {
	r.shutdown()

	// Verify all connections are closed before closing DB
	if conns := r.DB.Stats().OpenConnections; conns != 0 {
		require.FailNow(r.t, ErrOpenConnections{
			Database:       "mysql",
			NumConnections: conns,
		}.Error())
	}

	_, err := r.DB.Exec(fmt.Sprintf("drop database %s", r.name))
	if err != nil {
		return err
	}

	if err := r.DB.Close(); err != nil {
		return err
	}

	return nil
}

// CreateTestMySQLDB returns a TestMySQLDB which can be used in tests
// as a clean mysql database. All migrations are ran on the db before.
//
// Callers should call close on the returned *TestMySQLDB.
func CreateTestMySQLDB(t *testing.T) *TestMySQLDB {
	if testing.Short() {
		t.Skip("-short flag enabled")
	}
	if !docker.Enabled() {
		t.Skip("Docker not enabled")
	}

	containerName := "mysql-test-db"
	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	mysqlConfig := MySQLConfig{
		User:     "moov",
		Password: "secret",
	}

	resource, err := findOrLaunchMySQLContainer(pool, containerName, mysqlConfig)
	require.NoError(t, err)
	mysqlConfig.Address = fmt.Sprintf("tcp(localhost:%s)", resource.GetPort("3306/tcp"))

	dbName, err := CreateTemporaryDatabase(mysqlConfig)
	require.NoError(t, err)

	dbURL := fmt.Sprintf("%s:%s@%s/%s",
		mysqlConfig.User,
		mysqlConfig.Password,
		mysqlConfig.Address,
		dbName,
	)

	var db *sql.DB
	err = pool.Retry(func() error {
		db, err := sql.Open("mysql", dbURL)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
	if err != nil {
		resource.Close()
		require.FailNow(t, err.Error())
	}

	logger := log.NewNopLogger()
	ctx, cancelFunc := context.WithCancel(context.Background())
	dbConfig := DatabaseConfig{
		DatabaseName: dbName,
		MySQL:        &mysqlConfig,
	}
	db, err = NewAndMigrate(ctx, logger, dbConfig)
	require.NoError(t, err)

	// Don't allow idle connections so we can verify all are closed at the end of testing
	db.SetMaxIdleConns(0)

	return &TestMySQLDB{
		DB:       db,
		name:     dbName,
		shutdown: cancelFunc,
		t:        t,
	}
}

// We connect as root to MySQL server and create database with random name to
// run our migrations on it later.
func CreateTemporaryDatabase(config MySQLConfig) (string, error) {
	dsn := fmt.Sprintf("%s:%s@%s/", "root", config.Password, config.Address)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()

	dbName := "test" + base.ID()

	_, err = db.ExecContext(context.Background(), fmt.Sprintf("create database %s", dbName))
	if err != nil {
		return "", err
	}

	_, err = db.ExecContext(context.Background(), fmt.Sprintf("grant all on %s.* to '%s'@'%%'", dbName, config.User))
	if err != nil {
		return "", err
	}

	return dbName, nil
}

func findOrLaunchMySQLContainer(pool *dockertest.Pool, containerName string, config MySQLConfig) (*dockertest.Resource, error) {
	var resource *dockertest.Resource
	var err error

	_, err = pool.RunWithOptions(&dockertest.RunOptions{
		Name:       containerName,
		Repository: "vaulty/mysql-volumeless",
		Tag:        "8.0",
		Env: []string{
			fmt.Sprintf("MYSQL_USER=%s", config.User),
			fmt.Sprintf("MYSQL_PASSWORD=%s", config.Password),
			fmt.Sprintf("MYSQL_ROOT_PASSWORD=%s", config.Password),
		},
	})

	if err != nil && !errors.Is(err, dc.ErrContainerAlreadyExists) {
		return nil, err
	}

	// look for running container
	resource, found := pool.ContainerByName(containerName)
	if !found {
		return nil, errors.New("failed to launch (or find) MySQL container")
	}

	return resource, nil
}

// MySQLUniqueViolation returns true when the provided error matches the MySQL code
// for duplicate entries (violating a unique table constraint).
func MySQLUniqueViolation(err error) bool {
	match := strings.Contains(err.Error(), fmt.Sprintf("Error %d: Duplicate entry", mySQLErrDuplicateKey))
	if e, ok := err.(*gomysql.MySQLError); ok {
		return match || e.Number == mySQLErrDuplicateKey
	}
	return match
}
