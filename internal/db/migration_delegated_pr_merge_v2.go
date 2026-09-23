package db

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

const (
	delegatedPRMergeIntentIndexName = "idx_pr_action_intent_merge_delegation"
	delegatedPRMergeIntentTable     = "pull_request_action_intents"
	delegatedPRMergeIntentColumn    = "delegation_id"
)

// MigrateDelegatedPRMergeV2 is the additive retrofit for databases created
// before exact delegated merge authority and durable one-shot intent lineage.
// Historical rows remain null/empty and therefore cannot be interpreted as v2
// authorization evidence. There is intentionally no destructive down path.
func MigrateDelegatedPRMergeV2(database *gorm.DB) error {
	if database == nil {
		return fmt.Errorf("migrate delegated PR merge v2: database is nil")
	}
	migrator := database.Migrator()
	for _, field := range []string{
		"MergeDelegationID", "MergeDelegationRevision", "MergeDelegationNotAfter", "MergeDelegationFactsDigest",
		"MergeDelegationTargetInstance", "MergeDelegationCanonicalRepositoryID", "MergeDelegationProvider",
		"MergeDelegationProviderBindingID", "MergeDelegationProviderBindingRevision", "MergeDelegationProviderRepository",
		"MergeDelegationAGSPRNumber", "MergeDelegationProviderPRNumber", "MergeDelegationExpectedHeadSHA",
		"MergeDelegationExpectedBaseSHA", "MergeDelegationBaseRef", "MergeDelegationMethod", "MergeDelegationProjectionRevision",
	} {
		if !migrator.HasColumn(&DelegatedAgentSession{}, field) {
			if err := migrator.AddColumn(&DelegatedAgentSession{}, field); err != nil {
				return fmt.Errorf("add delegated session v2 column %s: %w", field, err)
			}
		}
	}
	for _, field := range []string{
		"ProviderOutcome", "MergeMethod", "DelegationID", "DelegationRevision", "DelegationNotAfter",
		"DelegationFactsDigest", "DelegationState", "DelegationConsumeReceiptID", "DelegationConsumedAt",
	} {
		if !migrator.HasColumn(&PullRequestActionIntent{}, field) {
			if err := migrator.AddColumn(&PullRequestActionIntent{}, field); err != nil {
				return fmt.Errorf("add merge intent v2 column %s: %w", field, err)
			}
		}
	}
	return ensureExactDelegatedPRMergeIntentIndex(database)
}

// delegatedMergeIndexDefinition is an exact, dialect-owned catalog proof. A
// bit is affirmative only when the native catalog supplied it; zero values are
// never treated as evidence.
type delegatedMergeIndexDefinition struct {
	Name, Table                         string
	Columns                             []string
	Unique, Valid, Ready, Visible       bool
	NoPrefix, NoExpression, NoPredicate bool
	Proven                              bool
}

func ensureExactDelegatedPRMergeIntentIndex(database *gorm.DB) error {
	driver := database.Dialector.Name()
	definitions, err := delegatedMergeIndexDefinitions(database)
	if err != nil {
		return fmt.Errorf("%w: native catalog proof unavailable: %v", delegatedMergeIndexOperatorError(driver), err)
	}
	found, correct := exactDelegatedMergeIndexDefinition(definitions)
	if found && !correct {
		return delegatedMergeIndexOperatorError(driver)
	}
	if found {
		return nil
	}
	if err := database.Exec("CREATE UNIQUE INDEX " + delegatedPRMergeIntentIndexName + " ON " + delegatedPRMergeIntentTable + " (" + delegatedPRMergeIntentColumn + ")").Error; err != nil {
		return fmt.Errorf("create delegated merge intent unique index: %w", err)
	}
	definitions, err = delegatedMergeIndexDefinitions(database)
	if err != nil {
		return fmt.Errorf("%w: created index native catalog proof unavailable: %v", delegatedMergeIndexOperatorError(driver), err)
	}
	found, correct = exactDelegatedMergeIndexDefinition(definitions)
	if !found || !correct {
		return fmt.Errorf("%w: created index could not be proven exact", delegatedMergeIndexOperatorError(driver))
	}
	return nil
}

func delegatedMergeIndexDefinitions(database *gorm.DB) ([]delegatedMergeIndexDefinition, error) {
	switch database.Dialector.Name() {
	case "sqlite":
		return delegatedMergeSQLiteIndexDefinitions(database)
	case "mysql":
		return delegatedMergeMySQLIndexDefinitions(database)
	case "postgres":
		return delegatedMergePostgresIndexDefinitions(database)
	default:
		return nil, fmt.Errorf("unsupported database dialect %q", database.Dialector.Name())
	}
}

func delegatedMergeSQLiteIndexDefinitions(database *gorm.DB) ([]delegatedMergeIndexDefinition, error) {
	var artifact struct {
		Name, TableName string
	}
	result := database.Raw(`SELECT name, tbl_name AS table_name FROM sqlite_master WHERE type = 'index' AND lower(name) = lower(?)`, delegatedPRMergeIntentIndexName).Scan(&artifact)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	definition := delegatedMergeIndexDefinition{Name: artifact.Name, Table: artifact.TableName, Valid: true, Ready: true, Visible: true, NoPrefix: true, NoPredicate: true, Proven: true}
	if !strings.EqualFold(artifact.TableName, delegatedPRMergeIntentTable) {
		return []delegatedMergeIndexDefinition{definition}, nil
	}
	rows, err := safePragmaRows(database, "index_list", delegatedPRMergeIntentTable)
	if err != nil {
		return nil, err
	}
	found := false
	for rows.Next() {
		var seq, unique, partial int
		var name string
		var origin sql.NullString
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.EqualFold(name, delegatedPRMergeIntentIndexName) {
			found = true
			definition.Unique = unique == 1
			definition.NoPredicate = partial == 0
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if !found {
		return []delegatedMergeIndexDefinition{definition}, nil
	}
	rows, err = safePragmaRows(database, "index_xinfo", delegatedPRMergeIntentIndexName)
	if err != nil {
		return nil, err
	}
	definition.NoExpression = true
	for rows.Next() {
		var seqNo, cid, desc, key int
		var name, collation sql.NullString
		if err := rows.Scan(&seqNo, &cid, &name, &desc, &collation, &key); err != nil {
			rows.Close()
			return nil, err
		}
		if key != 1 {
			continue
		}
		if cid < 0 || !name.Valid || strings.TrimSpace(name.String) == "" {
			definition.NoExpression = false
			continue
		}
		definition.Columns = append(definition.Columns, name.String)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return []delegatedMergeIndexDefinition{definition}, nil
}

func delegatedMergeMySQLIndexDefinitions(database *gorm.DB) ([]delegatedMergeIndexDefinition, error) {
	rows, err := database.Raw("SHOW INDEX FROM `" + delegatedPRMergeIntentTable + "`").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	catalogColumns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	positions := make(map[string]int, len(catalogColumns))
	for i, column := range catalogColumns {
		positions[strings.ToLower(column)] = i
	}
	for _, required := range []string{"table", "non_unique", "key_name", "seq_in_index", "column_name", "sub_part", "visible", "expression"} {
		if _, ok := positions[required]; !ok {
			return nil, fmt.Errorf("SHOW INDEX omitted required %s metadata", required)
		}
	}
	type part struct {
		seq    int
		column string
	}
	var parts []part
	definition := delegatedMergeIndexDefinition{Name: delegatedPRMergeIntentIndexName, Table: delegatedPRMergeIntentTable, Valid: true, Ready: true, NoPredicate: true, Proven: true, Unique: true, Visible: true, NoPrefix: true, NoExpression: true}
	found := false
	for rows.Next() {
		values := make([]any, len(catalogColumns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		keyName := catalogText(values[positions["key_name"]])
		if !strings.EqualFold(keyName, delegatedPRMergeIntentIndexName) {
			continue
		}
		found = true
		definition.Table = catalogText(values[positions["table"]])
		definition.Unique = catalogText(values[positions["non_unique"]]) == "0"
		seq, parseErr := strconv.Atoi(catalogText(values[positions["seq_in_index"]]))
		column := catalogText(values[positions["column_name"]])
		if parseErr != nil || seq < 1 || column == "" {
			definition.Proven = false
		} else {
			parts = append(parts, part{seq: seq, column: column})
		}
		definition.NoPrefix = definition.NoPrefix && catalogText(values[positions["sub_part"]]) == ""
		definition.Visible = definition.Visible && strings.EqualFold(catalogText(values[positions["visible"]]), "YES")
		definition.NoExpression = definition.NoExpression && catalogText(values[positions["expression"]]) == ""
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].seq < parts[j].seq })
	for i, item := range parts {
		if item.seq != i+1 {
			definition.Proven = false
		}
		definition.Columns = append(definition.Columns, item.column)
	}
	return []delegatedMergeIndexDefinition{definition}, nil
}

func delegatedMergePostgresIndexDefinitions(database *gorm.DB) ([]delegatedMergeIndexDefinition, error) {
	type catalogRow struct {
		TableName, IndexName, ColumnName, KeyDefinition string
		IsUnique, IsValid, IsReady                      bool
		NoPredicate, NoExpression                       bool
		KeyAttributeCount, AttributeCount, Ordinality   int
	}
	var rows []catalogRow
	result := database.Raw(`
SELECT tbl.relname AS table_name,
       idx.relname AS index_name,
       pi.indisunique AS is_unique,
       pi.indisvalid AS is_valid,
       pi.indisready AS is_ready,
       pi.indpred IS NULL AS no_predicate,
       pi.indexprs IS NULL AS no_expression,
       pi.indnkeyatts AS key_attribute_count,
       pi.indnatts AS attribute_count,
       ord.ordinality AS ordinality,
       COALESCE(att.attname, '') AS column_name,
       pg_get_indexdef(pi.indexrelid, ord.ordinality, true) AS key_definition
FROM pg_catalog.pg_index pi
JOIN pg_catalog.pg_class idx ON idx.oid = pi.indexrelid
JOIN pg_catalog.pg_namespace ns ON ns.oid = idx.relnamespace
JOIN pg_catalog.pg_class tbl ON tbl.oid = pi.indrelid
CROSS JOIN LATERAL unnest(pi.indkey) WITH ORDINALITY AS ord(attnum, ordinality)
LEFT JOIN pg_catalog.pg_attribute att ON att.attrelid = tbl.oid AND att.attnum = ord.attnum AND ord.attnum > 0
WHERE ns.nspname = current_schema() AND lower(idx.relname) = lower(?)
ORDER BY ord.ordinality`, delegatedPRMergeIntentIndexName).Scan(&rows)
	if result.Error != nil {
		return nil, result.Error
	}
	if len(rows) == 0 {
		return nil, nil
	}
	first := rows[0]
	definition := delegatedMergeIndexDefinition{Name: first.IndexName, Table: first.TableName, Unique: first.IsUnique, Valid: first.IsValid, Ready: first.IsReady,
		Visible: true, NoPrefix: true, NoExpression: first.NoExpression, NoPredicate: first.NoPredicate, Proven: true}
	if first.KeyAttributeCount != 1 || first.AttributeCount != 1 || len(rows) != 1 {
		definition.Proven = false
	}
	for i, row := range rows {
		if row.TableName != first.TableName || row.IndexName != first.IndexName || row.IsUnique != first.IsUnique || row.IsValid != first.IsValid || row.IsReady != first.IsReady || row.NoPredicate != first.NoPredicate || row.NoExpression != first.NoExpression || row.Ordinality != i+1 {
			definition.Proven = false
		}
		key := strings.Trim(strings.TrimSpace(row.KeyDefinition), `"`)
		if row.ColumnName == "" || !strings.EqualFold(key, row.ColumnName) {
			definition.NoExpression = false
		}
		definition.Columns = append(definition.Columns, row.ColumnName)
	}
	return []delegatedMergeIndexDefinition{definition}, nil
}

func catalogText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []byte:
		return strings.TrimSpace(string(typed))
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func exactDelegatedMergeIndexDefinition(definitions []delegatedMergeIndexDefinition) (found, correct bool) {
	for _, definition := range definitions {
		if !strings.EqualFold(definition.Name, delegatedPRMergeIntentIndexName) {
			continue
		}
		return true, definition.Proven && strings.EqualFold(definition.Table, delegatedPRMergeIntentTable) && definition.Unique && definition.Valid && definition.Ready && definition.Visible &&
			definition.NoPrefix && definition.NoExpression && definition.NoPredicate && len(definition.Columns) == 1 && strings.EqualFold(definition.Columns[0], delegatedPRMergeIntentColumn)
	}
	return false, false
}

func delegatedMergeIndexOperatorError(driver string) error {
	return fmt.Errorf("delegated PR merge v2 migration blocked on %s: index %s must be a visible, valid, ready, non-partial, non-expression UNIQUE %s(%s) with no prefix; AGS did not alter the existing artifact; an operator must rename or remove the incorrect index after review and rerun migration",
		driver, delegatedPRMergeIntentIndexName, delegatedPRMergeIntentTable, delegatedPRMergeIntentColumn)
}
