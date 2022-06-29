package azuread

import (
	"context"
	cryptotls "crypto/tls"
	"database/sql"
	"fmt"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/denisenkom/go-mssqldb/azuread"
	"github.com/denisenkom/go-mssqldb/msdsn"
	"github.com/go-sql-driver/azuread"

	"github.com/k3s-io/kine/pkg/drivers/generic"
	"github.com/k3s-io/kine/pkg/logstructured"
	"github.com/k3s-io/kine/pkg/logstructured/sqllog"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/k3s-io/kine/pkg/tls"
	"github.com/k3s-io/kine/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

const (
	defaultHostDSN = "root@tcp(127.0.0.1)/"
)

var (
	schema = []string{
		`CREATE TABLE IF NOT EXISTS kine
			(
				id INTEGER AUTO_INCREMENT,
				name VARCHAR(630),
				created INTEGER,
				deleted INTEGER,
				create_revision INTEGER,
 				prev_revision INTEGER,
				lease INTEGER,
				value MEDIUMBLOB,
				old_value MEDIUMBLOB,
				PRIMARY KEY (id)
			);`,
		`CREATE INDEX kine_name_index ON kine (name)`,
		`CREATE INDEX kine_name_id_index ON kine (name,id)`,
		`CREATE INDEX kine_id_deleted_index ON kine (id,deleted)`,
		`CREATE INDEX kine_prev_revision_index ON kine (prev_revision)`,
		`CREATE UNIQUE INDEX kine_name_prev_revision_uindex ON kine (name, prev_revision)`,
	}
	createDB = "CREATE DATABASE IF NOT EXISTS "
)

func New(ctx context.Context, dataSourceName string, tlsInfo tls.Config, connPoolConfig generic.ConnectionPoolConfig, metricsRegisterer prometheus.Registerer) (server.Backend, error) {
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}

	if tlsConfig != nil {
		tlsConfig.MinVersion = cryptotls.VersionTLS11
	}

	parsedDSN, err := prepareDSN(dataSourceName, tlsConfig)
	if err != nil {
		return nil, err
	}

	if err := createDBIfNotExist(parsedDSN); err != nil {
		return nil, err
	}

	dialect, err := generic.Open(ctx, azuread.DriverName, parsedDSN, connPoolConfig, "?", false, metricsRegisterer)
	if err != nil {
		return nil, err
	}

	dialect.LastInsertID = true
	dialect.GetSizeSQL = `
		SELECT SUM(data_length + index_length)
		FROM information_schema.TABLES
		WHERE table_schema = DATABASE() AND table_name = 'kine'`
	dialect.CompactSQL = `
		DELETE kv FROM kine AS kv
		INNER JOIN (
			SELECT kp.prev_revision AS id
			FROM kine AS kp
			WHERE
				kp.name != 'compact_rev_key' AND
				kp.prev_revision != 0 AND
				kp.id <= ?
			UNION
			SELECT kd.id AS id
			FROM kine AS kd
			WHERE
				kd.deleted != 0 AND
				kd.id <= ?
		) AS ks
		ON kv.id = ks.id`
	dialect.TranslateErr = func(err error) error {
		if _, ok := err.(*mssql.ServerError); ok {
			return server.ErrKeyExists
		}
		return err
	}
	dialect.ErrCode = func(err error) string {
		if err == nil {
			return ""
		}
		if err, ok := err.(*mssql.ServerError); ok {
			return fmt.Sprint(err)
		}
		return err.Error()
	}
	if err := setup(dialect.DB); err != nil {
		return nil, err
	}

	dialect.Migrate(context.Background())
	return logstructured.New(sqllog.New(dialect)), nil
}

func setup(db *sql.DB) error {
	logrus.Infof("Configuring database table schema and indexes, this may take a moment...")

	for _, stmt := range schema {
		logrus.Tracef("SETUP EXEC : %v", util.Stripped(stmt))
		_, err := db.Exec(stmt)
		if err != nil {
			if _, ok := err.(*mssql.ServerError); !ok {
				return err
			}
		}
	}

	logrus.Infof("Database tables and indexes are up to date")
	return nil
}

func createDBIfNotExist(dataSourceName string) error {
	config, _, err := msdsn.Parse(dataSourceName)
	if err != nil {
		return err
	}
	dbName := config.Database

	db, err := sql.Open(azuread.DriverName, dataSourceName)
	if err != nil {
		return err
	}
	_, err = db.Exec(createDB + dbName)
	if err != nil {
		if _, ok := err.(*mssql.ServerError); !ok {
			return err
		}
		config.Database = ""
		db, err = sql.Open(azuread.DriverName, config.URL().String())
		if err != nil {
			return err
		}
		_, err = db.Exec(createDB + dbName)
		if err != nil {
			return err
		}
	}
	return nil
}

func prepareDSN(dataSourceName string, tlsConfig *cryptotls.Config) (string, error) {
	if len(dataSourceName) == 0 {
		// FixMe: ...
		return "", nil
	}
	config, _, err := msdsn.Parse(dataSourceName)
	if err != nil {
		return "", err
	}
	// setting up tlsConfig
	/*
		if tlsConfig != nil {
			if err := azuread.RegisterTLSConfig("kine", tlsConfig); err != nil {
				return "", err
			}
			config.TLSConfig = "kine"
		}
	*/
	dbName := "kubernetes"
	if len(config.Database) > 0 {
		dbName = config.Database
	}
	config.Database = dbName
	parsedDSN := config.URL().String()

	return parsedDSN, nil
}
