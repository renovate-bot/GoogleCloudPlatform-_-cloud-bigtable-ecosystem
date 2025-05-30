/*
 * Copyright (C) 2025 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy of
 * the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
 * License for the specific language governing permissions and limitations under
 * the License.
 */

package translator

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	schemaMapping "github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/schema-mapping"
	cql "github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/third_party/cqlparser"
	"github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/utilities"
	"github.com/antlr4-go/antlr/v4"
	"github.com/datastax/go-cassandra-native-protocol/primitive"
)

// parseAssignments() processes a list of assignment elements from a CQL update statement,
// generating a structured response that includes the assignments' details and their corresponding
// placeholder values for parameterized queries.
//
// Parameters:
//   - assignments: A slice of IAssignmentElementContext, each representing an assignment in the CQL query.
//   - tableName: The name of the table being updated.
//   - schemaMapping: A pointer to SchemaMappingConfig holding schema information for the table.
//   - keyspace: The name of the keyspace containing the table.
//
// Returns:
//   - A pointer to an UpdateSetResponse which contains structured information about the assignments,
//     including updated values, parameter keys, and parameters map.
//   - An error if invalid input is detected, such as an empty assignment list or issues with assignment syntax,
//     or if a column type cannot be retrieved from the schema mapping table, or if an attempt is made to assign
//     a value to a primary key.
func parseAssignments(assignments []cql.IAssignmentElementContext, tableName string, schemaMapping *schemaMapping.SchemaMappingConfig, keyspace string, prependColumn *[]string) (*UpdateSetResponse, error) {
	if len(assignments) == 0 {
		return nil, errors.New("invalid input")
	}
	var setResp []UpdateSetValue
	var paramKeys []string
	params := make(map[string]interface{})

	for i, setVal := range assignments {
		assignStr := setVal.GetText()
		strSplit := strings.Split(assignStr, "=")
		s := strconv.Itoa(i + 1)
		placeholder := "set" + s
		colObj := setVal.OBJECT_NAME(0)
		if colObj == nil {
			return nil, errors.New("error parsing column for assignments")
		}
		columnName := colObj.GetText()
		if columnName == "" {
			return nil, errors.New("no columnName found for assignments")
		}
		columnName = strings.ReplaceAll(columnName, literalPlaceholder, "")
		var value string
		if strings.Contains(assignStr, "=?") {
			value = questionMark
		} else {
			valConst := setVal.Constant()
			if valConst == nil {
				value = strSplit[1]
				if value == "" {
					return nil, errors.New("error parsing value for assignments")
				}
			} else {
				value = valConst.GetText()
			}

		}

		value = strings.ReplaceAll(value, "'", "")
		var val interface{} //encryted val
		columnType, err := schemaMapping.GetColumnType(keyspace, tableName, columnName)
		if err != nil {
			return nil, fmt.Errorf("undefined column name %s in table %s.%s", columnName, keyspace, tableName)
		}
		if columnType.IsPrimaryKey {
			return nil, fmt.Errorf("primary key not allowed to assignments")
		}
		if value != questionMark {
			if columnType.IsCollection {
				val = value
			} else {
				val, err = formatValues(value, columnType.CQLType, 4)
				if err != nil {
					return nil, err
				}
			}
			params[placeholder] = val
		}
		paramKeys = append(paramKeys, placeholder)
		if (strings.Contains(columnType.CQLType, "map") || strings.Contains(columnType.CQLType, "list")) && IsMapKey(strSplit[0]) {
			_, extractkey := ExtractMapKey(assignStr)
			if extractkey == "" {
				return nil, fmt.Errorf("invalid format: missing map key")
			}
			newVal := fmt.Sprintf("%s+{%s:%s}", columnName, extractkey, value)
			val = newVal
		} else if strings.Contains(columnType.CQLType, "list") && strings.Contains(value, fmt.Sprintf("+%s", columnName)) {
			listAssignment := strSplit[1]
			valSplit := strings.Split(listAssignment, "+")
			newVal := fmt.Sprintf("%s+%s", valSplit[1], valSplit[0])
			val = newVal
			*prependColumn = append(*prependColumn, columnName)
		}
		setResp = append(setResp, UpdateSetValue{
			Column:    columnName,
			Value:     "@" + placeholder,
			Encrypted: val,
			CQLType:   columnType.CQLType,
		})
	}
	return &UpdateSetResponse{
		UpdateSetValues: setResp,
		ParamKeys:       paramKeys,
		Params:          params,
	}, nil
}

// TranslateUpdateQuerytoBigtable() frames UpdateQueryMapping which translates the update to bigtable mutation
//
// Parameters:
//   - query: CQL Update query
//
// Returns: UpdateQueryMapping struct and error if any
func (t *Translator) TranslateUpdateQuerytoBigtable(queryStr string, isPreparedQuery bool) (*UpdateQueryMapping, error) {
	lowerQuery := strings.ToLower(queryStr)
	query := renameLiterals(queryStr)

	query, PrependColumns := TransformPrepareQueryForPrependList(query)
	lexer := cql.NewCqlLexer(antlr.NewInputStream(query))
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := cql.NewCqlParser(stream)
	updateObj := p.Update()

	if updateObj == nil {
		return nil, errors.New("error parsing the update object")
	}

	kwUpdateObj := updateObj.KwUpdate()

	if kwUpdateObj == nil {
		return nil, errors.New("error parsing the update object")
	}

	keyspace := updateObj.Keyspace()
	updateObj.DOT()

	table := updateObj.Table()

	if table == nil || keyspace == nil {
		return nil, errors.New("invalid input paramaters found for table or keyspace")
	}

	keyspaceObj := keyspace.OBJECT_NAME()
	if keyspaceObj == nil {
		return nil, errors.New("invalid input paramaters found for keyspace")
	}
	keyspaceName := keyspaceObj.GetText()

	if !t.SchemaMappingConfig.InstanceExists(keyspaceName) {
		return nil, fmt.Errorf("keyspace %s does not exist", keyspaceName)
	}

	if !t.SchemaMappingConfig.TableExist(keyspaceName, table.GetText()) {
		return nil, fmt.Errorf("table %s does not exist", table.GetText())
	}

	tableobj := table.OBJECT_NAME()
	if tableobj == nil {
		return nil, errors.New("invalid input paramaters found for table")
	}
	tableName := tableobj.GetText()
	var err error

	timestampInfo, err := GetTimestampInfoByUpdate(queryStr, updateObj)
	if err != nil {
		return nil, err
	}
	updateObj.KwSet()
	assignmentObj := updateObj.Assignments()
	allAssignmentObj := assignmentObj.AllAssignmentElement()
	if allAssignmentObj == nil {
		return nil, errors.New("error parsing all the assignment object")
	}
	setValues, err := parseAssignments(allAssignmentObj, tableName, t.SchemaMappingConfig, keyspaceName, &PrependColumns)
	if err != nil {
		return nil, err
	}

	var QueryClauses QueryClauses

	if hasWhere(lowerQuery) {
		resp, err := parseWhereByClause(updateObj.WhereSpec(), tableName, t.SchemaMappingConfig, keyspaceName)
		if err != nil {
			return nil, err
		}

		QueryClauses = *resp

		for k, v := range QueryClauses.Params {
			setValues.Params[k] = v
		}

		setValues.ParamKeys = append(setValues.ParamKeys, QueryClauses.ParamKeys...)
	}
	ifExistObj := updateObj.IfExist()
	var ifExist bool = false
	if ifExistObj != nil {
		val := strings.ToLower(ifExistObj.GetText())
		if val == ifExists {
			ifExist = true
		}
	}

	primaryKeys, err := getPrimaryKeys(t.SchemaMappingConfig, tableName, keyspaceName)
	var actualPrimaryKeys []string
	for _, val := range QueryClauses.Clauses {
		actualPrimaryKeys = append(actualPrimaryKeys, val.Column)
	}
	if !ValidateRequiredPrimaryKeys(primaryKeys, actualPrimaryKeys) {
		missingPrime := findFirstMissingKey(primaryKeys, actualPrimaryKeys)
		missingPkColumnType, err := t.SchemaMappingConfig.GetPkKeyType(tableName, keyspaceName, missingPrime)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("some %s key parts are missing: %s", missingPkColumnType, missingPrime)
	}
	if err != nil {
		return nil, err
	}
	var primkeyvalues []string
	var rowKey string
	var values []interface{}
	var columns []Column
	for _, val := range setValues.UpdateSetValues {
		values = append(values, val.Encrypted)
		columns = append(columns, Column{Name: val.Column, ColumnFamily: t.SchemaMappingConfig.SystemColumnFamily, CQLType: val.CQLType})
	}
	var newValues []interface{} = values
	var newColumns []Column = columns
	var delColumns []Column
	var delColumnFamily []string
	var complexMeta map[string]*ComplexOperation
	var rawOutput *ProcessRawCollectionsOutput // Declare rawOutput here

	if !isPreparedQuery {
		pkValues := make(map[string]interface{})

		for _, key := range primaryKeys {
			for _, clause := range QueryClauses.Clauses {
				if !clause.IsPrimaryKey {
					return nil, errors.New("any value other then primary key is not accepted in where clause")
				}
				if clause.IsPrimaryKey && clause.Operator == "=" && key == clause.Column {
					pv := clause.Value
					if strings.HasPrefix(clause.Value, "@") {
						pv = clause.Value[1:]
					}
					pkValues[clause.Column] = QueryClauses.Params[pv]
					primkeyvalues = append(primkeyvalues, fmt.Sprintf("%v", QueryClauses.Params[pv]))
				}
			}
		}
		if len(primkeyvalues) != len(primaryKeys) {
			return nil, errors.New("missing primary key values in where clause")
		}
		pmks, err := t.SchemaMappingConfig.GetPkByTableNameWithFilter(tableName, keyspaceName, primaryKeys)
		if err != nil {
			return nil, err
		}
		rowKeyBytes, err := createOrderedCodeKey(pmks, pkValues, t.EncodeIntValuesWithBigEndian)
		if err != nil {
			return nil, err
		}
		rowKey = string(rowKeyBytes)

		// Building new colum family, qualifier and values for collection type of data.
		rawInput := ProcessRawCollectionsInput{
			Columns:        columns,
			Values:         values,
			TableName:      tableName,
			Translator:     t,
			KeySpace:       keyspaceName,
			PrependColumns: PrependColumns,
		}
		rawOutput, err = processCollectionColumnsForRawQueries(rawInput)
		if err != nil {
			return nil, fmt.Errorf("error processing raw collection columns: %w", err)
		}
		newColumns = rawOutput.NewColumns
		newValues = rawOutput.NewValues
		delColumnFamily = rawOutput.DelColumnFamily
		delColumns = rawOutput.DelColumns
		complexMeta = rawOutput.ComplexMeta // Assign complexMeta from output

		for _, val := range QueryClauses.Clauses {
			var column Column
			if columns, exists := t.SchemaMappingConfig.TablesMetaData[keyspaceName][tableName][val.Column]; exists {
				column = Column{Name: columns.ColumnName, ColumnFamily: t.SchemaMappingConfig.SystemColumnFamily, CQLType: columns.ColumnType}
			}
			newColumns = append(newColumns, column)

			pv := val.Value
			if strings.HasPrefix(val.Value, "@") {
				pv = val.Value[1:]
			}
			value := fmt.Sprintf("%v", QueryClauses.Params[pv])
			encryVal, err := formatValues(value, column.CQLType, 4)
			if err != nil {
				return nil, err
			}
			newValues = append(newValues, encryVal)
		}
	} else {
		complexMeta, err = t.ProcessComplexUpdate(columns, values, tableName, keyspaceName, PrependColumns)
		if err != nil {
			return nil, err
		}
	}

	updateQueryData := &UpdateQueryMapping{
		Query:                 query,
		QueryType:             UPDATE,
		Table:                 tableName,
		RowKey:                rowKey,
		Columns:               newColumns,
		Values:                newValues,
		DeleteColumnFamilies:  delColumnFamily,
		DeleteColumQualifires: delColumns,
		IfExists:              ifExist,
		Keyspace:              keyspaceName,
		Clauses:               QueryClauses.Clauses,
		Params:                setValues.Params,
		ParamKeys:             setValues.ParamKeys,
		UpdateSetValues:       setValues.UpdateSetValues,
		PrimaryKeys:           primaryKeys,
		TimestampInfo:         timestampInfo,
		ComplexOperation:      complexMeta,
	}

	return updateQueryData, nil
}

// BuildUpdatePrepareQuery() constructs an UpdateQueryMapping for an update operation, preparing the necessary
// components such as columns, values, row keys, and primary keys. It also processes any collection types
// and manages timestamp information for the update query.
//
// Parameters:
//   - columnsResponse: A slice of Column structs representing metadata of columns involved in the update.
//   - values: A slice of pointers to primitive.Value, representing the values for the update operation.
//   - st: A pointer to an UpdateQueryMapping that contains existing query data and metadata.
//   - protocolV: The Cassandra protocol version used for encoding and decoding operations.
//
// Returns:
//   - A pointer to an UpdateQueryMapping populated with the new query components required for the update.
//   - An error if any issues arise during processing, such as failure to fetch primary keys or errors
//     handling column data or timestamp.
func (t *Translator) BuildUpdatePrepareQuery(columnsResponse []Column, values []*primitive.Value, st *UpdateQueryMapping, protocolV primitive.ProtocolVersion) (*UpdateQueryMapping, error) {
	var newColumns []Column
	var newValues []interface{}
	var primaryKeys []string = st.PrimaryKeys
	var err error
	var unencrypted map[string]interface{}
	var delColumnFamily []string
	var delColumns []Column                            // Added missing declaration
	var prepareOutput *ProcessPrepareCollectionsOutput // Declare prepareOutput

	if len(primaryKeys) == 0 {
		primaryKeys, err = getPrimaryKeys(t.SchemaMappingConfig, st.Table, st.Keyspace)
		if err != nil {
			fmt.Println("Not able to fetch primary keys:", err)
			return nil, err
		}
	}
	timestampInfo, values, err := ProcessTimestampByUpdate(st, values)

	if err != nil {
		return nil, err
	}

	prepareInput := ProcessPrepareCollectionsInput{
		ColumnsResponse: columnsResponse,
		Values:          values,
		TableName:       st.Table,
		ProtocolV:       protocolV,
		PrimaryKeys:     primaryKeys,
		Translator:      t,
		KeySpace:        st.Keyspace,
		ComplexMeta:     st.ComplexOperation,
	}

	prepareOutput, err = processCollectionColumnsForPrepareQueries(prepareInput)
	if err != nil {
		return nil, err
	}
	newColumns = prepareOutput.NewColumns
	newValues = prepareOutput.NewValues
	unencrypted = prepareOutput.Unencrypted
	indexEnd := prepareOutput.IndexEnd
	delColumnFamily = prepareOutput.DelColumnFamily
	delColumns = prepareOutput.DelColumns

	for i, clause := range st.Clauses {
		var column Column
		if columns, exists := t.SchemaMappingConfig.TablesMetaData[st.Keyspace][st.Table][clause.Column]; exists {
			column = Column{Name: columns.ColumnName}
		}

		if slices.Contains(primaryKeys, column.Name) {

			val, _ := utilities.DecodeBytesToCassandraColumnType(values[i+indexEnd+1].Contents, st.VariableMetadata[i+indexEnd+1].Type, protocolV)
			unencrypted[column.Name] = val
		}
	}

	pmks, err := t.SchemaMappingConfig.GetPkByTableNameWithFilter(st.Table, st.Keyspace, primaryKeys)
	if err != nil {
		return nil, err
	}
	rowKeyBytes, err := createOrderedCodeKey(pmks, unencrypted, t.EncodeIntValuesWithBigEndian)
	if err != nil {
		return nil, err
	}
	rowKey := string(rowKeyBytes)

	UpdateQueryData := &UpdateQueryMapping{
		Query:                 st.Query,
		QueryType:             st.QueryType,
		Keyspace:              st.Keyspace,
		Columns:               newColumns,
		Values:                newValues,
		PrimaryKeys:           primaryKeys,
		RowKey:                rowKey,
		Table:                 st.Table,
		DeleteColumnFamilies:  delColumnFamily,
		DeleteColumQualifires: delColumns,
		Clauses:               st.Clauses,
		TimestampInfo:         timestampInfo,
		IfExists:              st.IfExists,
		ComplexOperation:      st.ComplexOperation,
	}

	return UpdateQueryData, nil
}
