from pyspark.sql import SparkSession
from pyspark.sql.types import StructType, StructField, LongType, StringType, TimestampType
from pyspark.sql.functions import col, concat, lit, when, sha2, rand, to_timestamp
import time
import math

# sql.parquet.writeLegacyFormat 优化大文件读写
def init_spark_session():
    """初始化Spark会话并配置MinIO连接"""
    spark = SparkSession.builder \
        .appName("Spark-MinIO-File-Test") \
        .config("spark.executor.memory", "8g") \
        .config("spark.driver.memory", "4g") \
        .config("spark.executor.cores", "4") \
        .config("spark.default.parallelism", "100") \
        .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000") \
        .config("spark.hadoop.fs.s3a.access.key", "minioadmin") \
        .config("spark.hadoop.fs.s3a.secret.key", "minioadmin") \
        .config("spark.hadoop.fs.s3a.path.style.access", "true") \
        .config("spark.hadoop.fs.s3a.impl", "org.apache.hadoop.fs.s3a.S3AFileSystem") \
        .config("spark.sql.parquet.writeLegacyFormat", "true") \
        .config("spark.sql.orc.filterPushdown", "true") \
        .getOrCreate()
    return spark

def generate_test_data(spark, num_rows):
    """生成测试数据"""
    # 定义Schema
    schema = StructType([
        StructField("id", LongType(), nullable=False),
        StructField("user_id", StringType(), nullable=False),
        StructField("event_time", TimestampType(), nullable=False),
        StructField("event_type", StringType(), nullable=False),
        StructField("device_id", StringType(), nullable=False),
        StructField("ip_address", StringType(), nullable=False),
        StructField("payload", StringType(), nullable=False)  # 约1KB的随机字符串
    ])
    
    # 生成基础数据
    base_data = spark.range(num_rows) \
        .withColumn("user_id", concat(lit("user_"), col("id") % 1000000)) \
        .withColumn("event_time", to_timestamp(lit("2023-01-01 00:00:00") + (col("id") % 31536000).cast("int"))) \
        .withColumn("event_type", 
            when(col("id") % 5 == 0, "click")
            .when(col("id") % 5 == 1, "view")
            .when(col("id") % 5 == 2, "purchase")
            .when(col("id") % 5 == 3, "login")
            .otherwise("logout")
        ) \
        .withColumn("device_id", concat(lit("device_"), col("id") % 10000)) \
        .withColumn("ip_address", concat(
            (col("id") % 255 + 1).cast("string"), lit("."),
            (col("id") % 255 + 1).cast("string"), lit("."),
            (col("id") % 255 + 1).cast("string"), lit("."),
            (col("id") % 255 + 1).cast("string")
        )) \
        .withColumn("payload", 
            sha2((rand().cast("string") + col("id").cast("string")), 256)
            .concat(sha2(rand().cast("string"), 256))
            .concat(sha2(rand().cast("string"), 256))
            .concat(sha2(rand().cast("string"), 256))
        )
    
    return base_data.select(
        col("id"), col("user_id"), col("event_time"), 
        col("event_type"), col("device_id"), 
        col("ip_address"), col("payload")
    )

def test_file_format(spark, data, path, file_format, is_small_file=False):
    """测试指定格式的读写性能和稳定性"""
    print(f"\n===== 测试{file_format.upper()}格式 {'(小文件)' if is_small_file else ''} =====")
    print(f"存储路径: {path}")
    
    # 写入性能测试
    start_write = time.time()
    write_options = {
        "mode": "overwrite",
        "compression": "snappy"  # 使用snappy压缩
    }
    
    if file_format == "parquet":
        data.write.parquet(path, **write_options)
    else:  # orc
        data.write.orc(path,** write_options)
    
    write_time = time.time() - start_write
    print(f"写入完成，耗时: {write_time:.2f}秒")
    
    # 读取性能测试
    start_read = time.time()
    if file_format == "parquet":
        df_read = spark.read.parquet(path)
    else:  # orc
        df_read = spark.read.orc(path)
    
    # 验证数据完整性
    original_count = data.count()
    read_count = df_read.count()
    if original_count == read_count:
        print(f"数据完整性验证通过: 原始行数={original_count}, 读取行数={read_count}")
    else:
        print(f"数据完整性验证失败: 原始行数={original_count}, 读取行数={read_count}")
    
    read_time = time.time() - start_read
    print(f"读取完成，耗时: {read_time:.2f}秒")
    
    # 简单查询测试
    start_query = time.time()
    df_read.groupBy("event_type").count().show()
    query_time = time.time() - start_query
    print(f"查询完成，耗时: {query_time:.2f}秒")
    
    return {
        "format": file_format,
        "is_small_file": is_small_file,
        "write_time": write_time,
        "read_time": read_time,
        "query_time": query_time,
        "row_count": original_count
    }

def test_large_files(spark, target_gb=5):
    """测试大文件场景（GB级）"""
    print(f"\n====== 开始大文件测试 (目标大小: {target_gb}GB) ======")
    # 估算行数（每行约1KB）
    rows_per_gb = 1024 * 1024  # 1GB ≈ 100万行
    total_rows = target_gb * rows_per_gb
    
    # 生成测试数据
    print(f"生成 {total_rows:,} 行数据 (约 {target_gb}GB)...")
    data = generate_test_data(spark, total_rows)
    
    # 测试Parquet
    parquet_path = "s3a://spark-test/large-files/parquet"
    parquet_result = test_file_format(spark, data, parquet_path, "parquet")
    
    # 测试ORC
    orc_path = "s3a://spark-test/large-files/orc"
    orc_result = test_file_format(spark, data, orc_path, "orc")
    
    return [parquet_result, orc_result]

def test_small_files(spark, file_count=1000000):
    """测试小文件场景（百万级）"""
    print(f"\n====== 开始小文件测试 (目标文件数: {file_count:,}) ======")
    # 每个文件1行数据
    print(f"生成 {file_count:,} 行数据 (每个文件1行)...")
    data = generate_test_data(spark, file_count)
    
    # 重分区确保生成指定数量的小文件
    data_repartitioned = data.repartition(file_count)
    
    # 测试Parquet
    parquet_path = "s3a://spark-test/small-files/parquet"
    parquet_result = test_file_format(spark, data_repartitioned, parquet_path, "parquet", is_small_file=True)
    
    # 测试ORC
    orc_path = "s3a://spark-test/small-files/orc"
    orc_result = test_file_format(spark, data_repartitioned, orc_path, "orc", is_small_file=True)
    
    return [parquet_result, orc_result]

def main():
    # 初始化Spark
    spark = init_spark_session()
    
    try:
        # 1. 大文件测试（5GB）
        large_file_results = test_large_files(spark, target_gb=5)
        
        # 2. 小文件测试（100万）
        # 注意：百万级小文件测试对系统压力较大，可先从10万开始
        small_file_results = test_small_files(spark, file_count=100000)
        
        print("\n====== 测试汇总 ======")
        for result in large_file_results + small_file_results:
            print(f"{result['format'].upper()} {'小文件' if result['is_small_file'] else '大文件'}:")
            print(f"  行数: {result['row_count']:,}")
            print(f"  写入时间: {result['write_time']:.2f}秒")
            print(f"  读取时间: {result['read_time']:.2f}秒")
            print(f"  查询时间: {result['query_time']:.2f}秒")
            print("  --------------------")
            
    finally:
        spark.stop()
        print("\nSpark会话已关闭")

if __name__ == "__main__":
    main()
