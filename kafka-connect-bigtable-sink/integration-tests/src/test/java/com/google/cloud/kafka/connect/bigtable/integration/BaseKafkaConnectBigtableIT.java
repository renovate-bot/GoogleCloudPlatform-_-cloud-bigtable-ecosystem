/*
 * Copyright 2025 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package com.google.cloud.kafka.connect.bigtable.integration;

import static org.apache.kafka.test.TestUtils.waitForCondition;

import com.google.api.gax.rpc.FailedPreconditionException;
import com.google.api.gax.rpc.NotFoundException;
import com.google.cloud.bigtable.admin.v2.models.ColumnFamily;
import com.google.cloud.bigtable.admin.v2.models.CreateTableRequest;
import com.google.cloud.bigtable.admin.v2.models.Table;
import com.google.cloud.bigtable.data.v2.BigtableDataClient;
import com.google.cloud.bigtable.data.v2.models.Query;
import com.google.cloud.bigtable.data.v2.models.Row;
import com.google.cloud.kafka.connect.bigtable.wrappers.BigtableTableAdminClientInterface;
import com.google.common.util.concurrent.Futures;
import com.google.protobuf.ByteString;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Future;
import java.util.function.Supplier;
import java.util.stream.Collectors;
import org.apache.kafka.test.TestCondition;
import org.junit.After;
import org.junit.Before;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

public abstract class BaseKafkaConnectBigtableIT extends BaseKafkaConnectIT {

  // Not copied from BigtableSinkConfig since it isn't present in its public API.
  public static long DEFAULT_BIGTABLE_RETRY_TIMEOUT_MILLIS = 90000;

  private final Logger logger = LoggerFactory.getLogger(BaseKafkaConnectBigtableIT.class);

  public BigtableDataClient bigtableData;
  public BigtableTableAdminClientInterface bigtableAdmin;

  @Before
  public void setUpBigtable() {
    Map<String, String> props = baseConnectorProps();
    bigtableData = getBigtableDataClient(props);
    bigtableAdmin = getBigtableAdminClient(props);
  }

  @After
  public void tearDownBigtable() {
    if (bigtableData != null) {
      bigtableData.close();
    }
    if (bigtableAdmin != null) {
      bigtableAdmin.close();
    }
  }

  public void createTablesAndColumnFamilies(String testId)
      throws ExecutionException, InterruptedException {
    createTablesAndColumnFamilies(Map.of(testId, Set.of(testId)));
  }

  public void createTablesAndColumnFamilies(Map<String, Set<String>> tablesAndColumnFamilies)
      throws ExecutionException, InterruptedException {
    List<Future<Table>> futures =
        tablesAndColumnFamilies.entrySet().parallelStream()
            .map(
                e -> {
                  CreateTableRequest ctr = CreateTableRequest.of(e.getKey());
                  e.getValue().forEach(ctr::addFamily);
                  return bigtableAdmin.createTableAsync(ctr);
                })
            .collect(Collectors.toList());
    for (Future<Table> f : futures) {
      f.get();
    }
  }

  public Map<ByteString, Row> readAllRows(BigtableDataClient bigtable, String table) {
    Integer numRecords = null;
    try {
      Query query = Query.create(table);
      Map<ByteString, Row> result =
          bigtable.readRows(query).stream().collect(Collectors.toMap(Row::getKey, r -> r));
      numRecords = result.size();
      return result;
    } catch (Throwable t) {
      throw t;
    } finally {
      logger.info("readAllRows({}): #records={}", table, numRecords);
    }
  }

  public long cellCount(Map<ByteString, Row> rows) {
    return rows.values().stream().mapToLong(r -> r.getCells().size()).sum();
  }

  public void waitUntilBigtableContainsNumberOfRows(String tableId, long numberOfRows)
      throws InterruptedException {
    waitForCondition(
        testConditionIgnoringTransientErrors(
            () -> readAllRows(bigtableData, tableId).size() == numberOfRows),
        DEFAULT_BIGTABLE_RETRY_TIMEOUT_MILLIS,
        "Records not consumed in time.");
  }

  public void waitUntilBigtableContainsNumberOfCells(String tableId, long numberOfCells)
      throws InterruptedException {
    waitForCondition(
        testConditionIgnoringTransientErrors(
            () -> cellCount(readAllRows(bigtableData, tableId)) == numberOfCells),
        DEFAULT_BIGTABLE_RETRY_TIMEOUT_MILLIS,
        "Records not consumed in time");
  }

  public void waitUntilBigtableTableExists(String tableId) throws InterruptedException {
    waitForCondition(
        testConditionIgnoringTransientErrors(
            () -> {
              bigtableAdmin.getTable(tableId);
              return true;
            }),
        DEFAULT_BIGTABLE_RETRY_TIMEOUT_MILLIS,
        "Table not created in time.");
  }

  public void waitUntilBigtableTableHasExactSetOfColumnFamilies(
      String tableId, Set<String> columnFamilies) throws InterruptedException {
    waitForCondition(
        testConditionIgnoringTransientErrors(
            () ->
                bigtableAdmin.getTable(tableId).getColumnFamilies()
                    .stream()
                    .map(ColumnFamily::getId)
                    .collect(Collectors.toSet())
                    .equals(columnFamilies)),
        DEFAULT_BIGTABLE_RETRY_TIMEOUT_MILLIS,
        "Column Families not created in time or an extra one created.");
  }

  // These exceptions are thrown around the moment of a table or column family creation.
  private TestCondition testConditionIgnoringTransientErrors(Supplier<Boolean> supplier) {
    return () -> {
      try {
        return supplier.get();
      } catch (NotFoundException | FailedPreconditionException e) {
        return false;
      }
    };
  }
}
