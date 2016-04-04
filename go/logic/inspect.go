/*
   Copyright 2016 GitHub Inc.
	 See https://github.com/github/gh-osc/blob/master/LICENSE
*/

package logic

import (
	gosql "database/sql"
	"fmt"
	"strings"

	"github.com/github/gh-osc/go/base"
	"github.com/github/gh-osc/go/mysql"
	"github.com/github/gh-osc/go/sql"

	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/sqlutils"
)

// Inspector reads data from the read-MySQL-server (typically a replica, but can be the master)
// It is used for gaining initial status and structure, and later also follow up on progress and changelog
type Inspector struct {
	connectionConfig *mysql.ConnectionConfig
	db               *gosql.DB
	migrationContext *base.MigrationContext
}

func NewInspector(connectionConfig *mysql.ConnectionConfig) *Inspector {
	return &Inspector{
		connectionConfig: connectionConfig,
		migrationContext: base.GetMigrationContext(),
	}
}

func (this *Inspector) InitDBConnections() (err error) {
	inspectorUri := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", this.connectionConfig.User, this.connectionConfig.Password, this.connectionConfig.Hostname, this.connectionConfig.Port, this.migrationContext.DatabaseName)
	if this.db, _, err = sqlutils.GetDB(inspectorUri); err != nil {
		return err
	}
	if err := this.validateConnection(); err != nil {
		return err
	}
	if err := this.validateGrants(); err != nil {
		return err
	}
	if err := this.validateBinlogs(); err != nil {
		return err
	}
	if err := this.validateTable(); err != nil {
		return err
	}
	if this.migrationContext.CountTableRows {
		if err := this.countTableRows(); err != nil {
			return err
		}
	} else {
		if err := this.estimateTableRowsViaExplain(); err != nil {
			return err
		}
	}

	return nil
}

func (this *Inspector) InspectTables() (err error) {
	uniqueKeys, err := this.getCandidateUniqueKeys(this.migrationContext.OriginalTableName)
	if err != nil {
		return err
	}
	if len(uniqueKeys) == 0 {
		return fmt.Errorf("No PRIMARY nor UNIQUE key found in table! Bailing out")
	}
	return nil
}

// validateConnection issues a simple can-connect to MySQL
func (this *Inspector) validateConnection() error {
	query := `select @@port`
	var port int
	if err := this.db.QueryRow(query).Scan(&port); err != nil {
		return err
	}
	if port != this.connectionConfig.Port {
		return fmt.Errorf("Unexpected database port reported: %+v", port)
	}
	log.Infof("connection validated on port %+v", port)
	return nil
}

// validateGrants verifies the user by which we're executing has necessary grants
// to do its thang.
func (this *Inspector) validateGrants() error {
	query := `show /* gh-osc */ grants for current_user()`
	foundAll := false
	foundSuper := false
	foundReplicationSlave := false
	foundDBAll := false

	err := sqlutils.QueryRowsMap(this.db, query, func(rowMap sqlutils.RowMap) error {
		for _, grantData := range rowMap {
			grant := grantData.String
			if strings.Contains(grant, `GRANT ALL PRIVILEGES ON *.*`) {
				foundAll = true
			}
			if strings.Contains(grant, `SUPER`) && strings.Contains(grant, ` ON *.*`) {
				foundSuper = true
			}
			if strings.Contains(grant, `REPLICATION SLAVE`) && strings.Contains(grant, ` ON *.*`) {
				foundReplicationSlave = true
			}
			if strings.Contains(grant, fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.*", this.migrationContext.DatabaseName)) {
				foundDBAll = true
			}
		}
		return nil
	})
	if err != nil {
		return log.Errore(err)
	}

	if foundAll {
		log.Infof("User has ALL privileges")
		return nil
	}
	if foundSuper && foundReplicationSlave && foundDBAll {
		log.Infof("User has SUPER, REPLICATION SLAVE privileges, and has ALL privileges on `%s`", this.migrationContext.DatabaseName)
		return nil
	}
	return log.Errorf("User has insufficient privileges for migration.")
}

// validateConnection issues a simple can-connect to MySQL
func (this *Inspector) validateBinlogs() error {
	query := `select @@global.log_bin, @@global.log_slave_updates, @@global.binlog_format`
	var hasBinaryLogs, logSlaveUpdates bool
	if err := this.db.QueryRow(query).Scan(&hasBinaryLogs, &logSlaveUpdates, &this.migrationContext.OriginalBinlogFormat); err != nil {
		return err
	}
	if !hasBinaryLogs {
		return fmt.Errorf("%s:%d must have binary logs enabled", this.connectionConfig.Hostname, this.connectionConfig.Port)
	}
	if !logSlaveUpdates {
		return fmt.Errorf("%s:%d must have log_slave_updates enabled", this.connectionConfig.Hostname, this.connectionConfig.Port)
	}
	if this.migrationContext.RequiresBinlogFormatChange() {
		query := fmt.Sprintf(`show /* gh-osc */ slave hosts`)
		countReplicas := 0
		err := sqlutils.QueryRowsMap(this.db, query, func(rowMap sqlutils.RowMap) error {
			countReplicas++
			return nil
		})
		if err != nil {
			return log.Errore(err)
		}
		if countReplicas > 0 {
			return fmt.Errorf("%s:%d has %s binlog_format, but I'm too scared to change it to ROW because it has replicas. Bailing out", this.connectionConfig.Hostname, this.connectionConfig.Port, this.migrationContext.OriginalBinlogFormat)
		}
		log.Infof("%s:%d has %s binlog_format. I will change it to ROW for the duration of this migration.", this.connectionConfig.Hostname, this.connectionConfig.Port, this.migrationContext.OriginalBinlogFormat)
	}
	query = `select @@global.binlog_row_image`
	if err := this.db.QueryRow(query).Scan(&this.migrationContext.OriginalBinlogRowImage); err != nil {
		// Only as of 5.6. We wish to support 5.5 as well
		this.migrationContext.OriginalBinlogRowImage = ""
	}

	log.Infof("binary logs validated on %s:%d", this.connectionConfig.Hostname, this.connectionConfig.Port)
	return nil
}

// validateTable makes sure the table we need to operate on actually exists
func (this *Inspector) validateTable() error {
	query := fmt.Sprintf(`show /* gh-osc */ table status from %s like '%s'`, sql.EscapeName(this.migrationContext.DatabaseName), this.migrationContext.OriginalTableName)

	tableFound := false
	err := sqlutils.QueryRowsMap(this.db, query, func(rowMap sqlutils.RowMap) error {
		this.migrationContext.TableEngine = rowMap.GetString("Engine")
		this.migrationContext.RowsEstimate = rowMap.GetInt64("Rows")
		this.migrationContext.UsedRowsEstimateMethod = base.TableStatusRowsEstimate
		if rowMap.GetString("Comment") == "VIEW" {
			return fmt.Errorf("%s.%s is a VIEW, not a real table. Bailing out", sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(this.migrationContext.OriginalTableName))
		}
		tableFound = true

		return nil
	})
	if err != nil {
		return log.Errore(err)
	}
	if !tableFound {
		return log.Errorf("Cannot find table %s.%s!", sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(this.migrationContext.OriginalTableName))
	}
	log.Infof("Table found. Engine=%s", this.migrationContext.TableEngine)
	log.Debugf("Estimated number of rows via STATUS: %d", this.migrationContext.RowsEstimate)
	return nil
}

func (this *Inspector) estimateTableRowsViaExplain() error {
	query := fmt.Sprintf(`explain select /* gh-osc */ * from %s.%s where 1=1`, sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(this.migrationContext.OriginalTableName))

	outputFound := false
	err := sqlutils.QueryRowsMap(this.db, query, func(rowMap sqlutils.RowMap) error {
		this.migrationContext.RowsEstimate = rowMap.GetInt64("rows")
		this.migrationContext.UsedRowsEstimateMethod = base.ExplainRowsEstimate
		outputFound = true

		return nil
	})
	if err != nil {
		return log.Errore(err)
	}
	if !outputFound {
		return log.Errorf("Cannot run EXPLAIN on %s.%s!", sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(this.migrationContext.OriginalTableName))
	}
	log.Infof("Estimated number of rows via EXPLAIN: %d", this.migrationContext.RowsEstimate)
	return nil
}

func (this *Inspector) countTableRows() error {
	log.Infof("As instructed, I'm issuing a SELECT COUNT(*) on the table. This may take a while")
	query := fmt.Sprintf(`select /* gh-osc */ count(*) as rows from %s.%s`, sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(this.migrationContext.OriginalTableName))
	if err := this.db.QueryRow(query).Scan(&this.migrationContext.RowsEstimate); err != nil {
		return err
	}
	this.migrationContext.UsedRowsEstimateMethod = base.CountRowsEstimate
	log.Infof("Exact number of rows via COUNT: %d", this.migrationContext.RowsEstimate)
	return nil
}

// getCandidateUniqueKeys investigates a table and returns the list of unique keys
// candidate for chunking
func (this *Inspector) getCandidateUniqueKeys(tableName string) (uniqueKeys [](*sql.UniqueKey), err error) {
	query := `
    SELECT
      COLUMNS.TABLE_SCHEMA,
      COLUMNS.TABLE_NAME,
      COLUMNS.COLUMN_NAME,
      UNIQUES.INDEX_NAME,
      UNIQUES.COLUMN_NAMES,
      UNIQUES.COUNT_COLUMN_IN_INDEX,
      COLUMNS.DATA_TYPE,
      COLUMNS.CHARACTER_SET_NAME,
      has_nullable
    FROM INFORMATION_SCHEMA.COLUMNS INNER JOIN (
      SELECT
        TABLE_SCHEMA,
        TABLE_NAME,
        INDEX_NAME,
        COUNT(*) AS COUNT_COLUMN_IN_INDEX,
        GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX ASC) AS COLUMN_NAMES,
        SUBSTRING_INDEX(GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX ASC), ',', 1) AS FIRST_COLUMN_NAME,
        SUM(NULLABLE='YES') > 0 AS has_nullable
      FROM INFORMATION_SCHEMA.STATISTICS
      WHERE NON_UNIQUE=0
      GROUP BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME
    ) AS UNIQUES
    ON (
      COLUMNS.TABLE_SCHEMA = UNIQUES.TABLE_SCHEMA AND
      COLUMNS.TABLE_NAME = UNIQUES.TABLE_NAME AND
      COLUMNS.COLUMN_NAME = UNIQUES.FIRST_COLUMN_NAME
    )
    WHERE
      COLUMNS.TABLE_SCHEMA = ?
      AND COLUMNS.TABLE_NAME = ?
    ORDER BY
      COLUMNS.TABLE_SCHEMA, COLUMNS.TABLE_NAME,
      CASE UNIQUES.INDEX_NAME
        WHEN 'PRIMARY' THEN 0
        ELSE 1
      END,
      CASE has_nullable
        WHEN 0 THEN 0
        ELSE 1
      END,
      CASE IFNULL(CHARACTER_SET_NAME, '')
          WHEN '' THEN 0
          ELSE 1
      END,
      CASE DATA_TYPE
        WHEN 'tinyint' THEN 0
        WHEN 'smallint' THEN 1
        WHEN 'int' THEN 2
        WHEN 'bigint' THEN 3
        ELSE 100
      END,
      COUNT_COLUMN_IN_INDEX
  `
	err = sqlutils.QueryRowsMap(this.db, query, func(rowMap sqlutils.RowMap) error {
		uniqueKey := &sql.UniqueKey{
			Name:        rowMap.GetString("INDEX_NAME"),
			Columns:     *sql.ParseColumnList(rowMap.GetString("COLUMN_NAMES")),
			HasNullable: rowMap.GetBool("has_nullable"),
		}
		uniqueKeys = append(uniqueKeys, uniqueKey)
		return nil
	}, this.migrationContext.DatabaseName, tableName)
	if err != nil {
		return uniqueKeys, err
	}
	log.Debugf("Potential unique keys: %+v", uniqueKeys)
	return uniqueKeys, nil
}

// getCandidateUniqueKeys investigates a table and returns the list of unique keys
// candidate for chunking
func (this *Inspector) getSharedUniqueKeys() (uniqueKeys [](*sql.UniqueKey), err error) {
	originalUniqueKeys, err := this.getCandidateUniqueKeys(this.migrationContext.OriginalTableName)
	if err != nil {
		return uniqueKeys, err
	}
	ghostUniqueKeys, err := this.getCandidateUniqueKeys(this.migrationContext.GhostTableName)
	if err != nil {
		return uniqueKeys, err
	}
	// We actually do NOT rely on key name, just on the set of columns. This is because maybe
	// the ALTER is on the name itself...
	for _, originalUniqueKey := range originalUniqueKeys {
		for _, ghostUniqueKey := range ghostUniqueKeys {
			if originalUniqueKey.Columns.Equals(&ghostUniqueKey.Columns) {
				uniqueKeys = append(uniqueKeys, originalUniqueKey)
			}
		}
	}
	return uniqueKeys, nil
}