# Overview
MinIO 与 Spark、Trino、Iceberg 等组件的集成方式，确保读写链路通畅。

## Iceberg && Minio
Iceberg 跟Minio是协同关系， Minio 提供底层存储能力，而Iceberg在其上构建表格式和元数据管理的能力，实现高效的海量数据管理
Iceberg本身不存储数据，当业务的数据存储在Minio上，Iceberg就负责把数据创建metadata索引等，为数据提供高效查询

## 创建Iceberg表并写入Minio
以 Spark 为例，演示如何配置 Iceberg 使用 MinIO 作为存储，并创建表、写入数据：
首先需在 Spark 配置中指定 MinIO 的访问信息和 Iceberg 的元数据存储路径
```scala
// Spark 配置示例（spark-defaults.conf 或代码中设置）
val spark = SparkSession.builder()
  .appName("Iceberg-MinIO-Demo")
  .config("spark.sql.extensions", "org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions")
  .config("spark.sql.catalog.spark_catalog", "org.apache.iceberg.spark.SparkSessionCatalog")
  .config("spark.sql.catalog.spark_catalog.type", "hive") // 可选：用 Hive 元数据或 Iceberg 自身元数据
  // MinIO 配置（兼容 S3 API）
  .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000") // MinIO 服务地址
  .config("spark.hadoop.fs.s3a.access.key", "minioadmin") // 访问密钥
  .config("spark.hadoop.fs.s3a.secret.key", "minioadmin") // 密钥
  .config("spark.hadoop.fs.s3a.path.style.access", "true") // 路径风格（必需）
  .config("spark.hadoop.fs.s3a.impl", "org.apache.hadoop.fs.s3a.S3AFileSystem")
  .getOrCreate()
```
创建Iceberg表
通过 SQL 或 DataFrame API 创建表，并指定路径为 MinIO 上的目录
```sql
-- 创建 Iceberg 表，指定存储路径为 MinIO（s3a 协议）
CREATE TABLE spark_catalog.default.iceberg_demo (
  id INT,
  name STRING,
  create_time TIMESTAMP
)
USING iceberg
LOCATION 's3a://iceberg-bucket/iceberg_demo' -- MinIO 中的 bucket 和路径
PARTITIONED BY (days(create_time)) -- 按时间分区
```

写入数据到Iceberg表，实际会存储到Minio中
```scala
// 生成示例数据
val data = Seq(
  (1, "Alice", "2023-10-01 10:00:00"),
  (2, "Bob", "2023-10-01 11:00:00")
).toDF("id", "name", "create_time")

// 写入 Iceberg 表
data.writeTo("spark_catalog.default.iceberg_demo").append()
```
 MinIO 中的数据结构
写入后，MinIO 的 iceberg-bucket/iceberg_demo 路径下会生成两类文件
```
iceberg_demo/
├── data/                   # 数据文件（Parquet 格式）
│   └── create_time_day=2023-10-01/
│       └── 00000-xxx.parquet
└── metadata/               # Iceberg 元数据文件
    ├── v1.metadata.json    # 表元数据版本 1
    ├── snap-xxx.avro       # 快照信息（记录数据文件列表）
    └── ...
```

Iceberg 不是运行在 MinIO 上的 “引擎”，而是基于 MinIO 等存储系统的表格式，负责管理数据的组织方式和查询逻辑。
利用 MinIO 的分布式存储能力实现数据高可用，同时通过 Iceberg 的表格式特性（如 ACID 事务、快照回滚、分区进化）简化海量数据的管理和分析。


## Trino && Minio
Trino 是分布式 SQL 查询引擎，提供 “高效查询数据” 的能力，不管数据是用 Iceberg 管理还是直接以文件形式存储在 MinIO 中，Trino 都能查询
Trino 与 MinIO 集成的核心目的是：
让用户能通过 SQL 直接查询 MinIO 中存储的文件（如 Parquet、CSV、ORC 等），无需将数据迁移到专门的数据库中。
举例若 MinIO 中保存了大量 Parquet 格式的用户行为日志（路径为 s3a://logs/2023/10/），Trino 可以直接通过 SQL 查询这些文件：
```sql
SELECT user_id, COUNT(*) 
FROM s3a.logs."2023/10/"  -- 直接查询 MinIO 中的路径
WHERE action = 'click' 
GROUP BY user_id;
```

为什么有了 Iceberg + MinIO，还需要 Trino + MinIO？
Iceberg 和 Trino 解决的是数据生命周期中不同阶段的问题，二者是互补关系而非替代关系


简单说：
Iceberg 是 “数据管理员”：它在 MinIO 上把零散的文件组织成 “表”，记录哪些数据有效、如何分区、历史版本是什么，确保数据的一致性和可管理性。
Trino 是 “数据查询者”：它不关心数据是怎么被管理的，只负责把用户的 SQL 转换成计算任务，从 MinIO 中读取数据（无论是 Iceberg 表还是裸文件）并返回结果。

三者通常一起工作，形成 “存储 - 管理 - 查询” 的完整链路：
数据写入：
* 通过 Spark/Flink 等工具，将数据以 Iceberg 表的形式写入 MinIO（Iceberg 负责组织元数据，MinIO 存储实际数据文件）。
* 数据管理：
借助 Iceberg 的特性对 MinIO 中的数据进行维护，例如：
新增列（Schema 演进）；
合并小文件（优化查询性能）；
回滚到历史版本（数据出错时）。
* 数据查询：
用户通过 Trino 提交 SQL 查询，Trino 会：
先访问 Iceberg 的元数据（存储在 MinIO 中），获取表结构、分区信息、数据文件路径；
根据元数据定位到 MinIO 中的具体数据文件（如 Parquet）；
并行读取并计算，返回查询结果。


## 配置Trino和Minio集成
在 Trino 中配置与 MinIO 的集成，核心是通过 Trino 的 Hive 连接器（最常用）或 S3 连接器 关联 MinIO 的对象存储，实现对 MinIO 中数据的查询。以下是详细的配置步骤和管理方法：
在 Trino 集群的 etc/catalog 目录下，创建一个以 .properties 结尾的配置文件（如 minio.properties），内容如下：
```
# 指定连接器类型为 Hive
connector.name=hive

# Hive 元数据存储（可选：若用 Hive Metastore 管理表结构，需配置；若直接查文件，可省略）
hive.metastore.uri=thrift://<hive-metastore-ip>:9083

# 配置 MinIO 作为底层存储（兼容 S3）
hive.s3.endpoint=http://<minio-ip>:9000  # MinIO 服务地址（http/https 需与 MinIO 配置一致）
hive.s3.path-style-access=true  # 启用路径风格访问（必需，MinIO 默认使用路径风格）
hive.s3.access-key=<your-minio-access-key>  # MinIO 的 Access Key
hive.s3.secret-key=<your-minio-secret-key>  # MinIO 的 Secret Key

# 可选：配置 S3 客户端参数（如超时时间）
hive.s3.connect-timeout=5s
hive.s3.read-timeout=10s

# 可选：指定默认文件格式（如 Parquet，查询时可省略格式声明）
hive.default-file-format=PARQUET
```

## 进阶配置：使用 S3 连接器（可选）
除了 Hive 连接器，Trino 还提供 S3 连接器（s3 类型），专门用于查询 S3/MinIO 中的对象存储文件，配置更简洁（无需依赖 Hive Metastore）。
创建 S3 连接器配置文件（etc/catalog/minio-s3.properties）
```
connector.name=s3

# MinIO 访问配置
s3.endpoint=http://<minio-ip>:9000
s3.path-style-access=true
s3.access-key=<your-minio-access-key>
s3.secret-key=<your-minio-secret-key>

# 配置默认文件格式和分割符（针对 CSV 等文本文件）
s3.default-file-format=PARQUET
s3.csv.separator=,
```

