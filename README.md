# Bigtable Ecosystem

This repository serves as a central hub for resources related to Google Cloud Bigtable, providing links to various tools, libraries, and documentation that contribute to the Bigtable Ecosystem.

## Contents

*   [Migration Tools](#migration-tools)

## Migrations Tools

*   **[DynamoDB to Bigtable Migration Tool](https://github.com/GoogleCloudPlatform/professional-services/tree/main/tools/dynamodb-bigtable-migration#bigtable-data-bridge---dynamodb-to-bigtable-migration-utility)** - The DynamoDB to Bigtable Migration tool is a powerful solution designed to streamline data transfer from DynamoDB to Cloud Bigtable. This tool automates schema translation, ensuring your data structure is mapped to Bigtable. It also provides options to accelerate and scale data transfer efficiently using Dataflow, minimizing downtime and maximizing performance.

*   **[Cassandra to Bigtable Migration Tool](https://github.com/GoogleCloudPlatform/DataflowTemplates/blob/main/v1/README_Cassandra_To_Cloud_Bigtable.md)** - The Apache Cassandra to Bigtable Dataflow template copies a table from Apache Cassandra to Bigtable. This template requires minimal configuration and replicates the table structure in Cassandra as closely as possible in Bigtable.

*  **[Bigtable HBase Replication Library](https://github.com/googleapis/java-bigtable-hbase/tree/main/hbase-migration-tools/bigtable-hbase-replication)** - Facilitate near-zero downtime migrations from HBase to Bigtable by enabling to keep your Bigtable instance in sync with your production HBase cluster. Adding Bigtable as an HBase replica guarantees that mutations are applied to Bigtable in the same order as on HBase.
  
*  **[HBase Sequence Files to Bigtable using Dataflow](https://github.com/googleapis/java-bigtable-hbase/blob/v2.15.0/bigtable-dataflow-parent/bigtable-beam-import/README.md)** - This folder contains tools to support importing and exporting HBase data to Bigtable using Dataflow and Apache beam.
