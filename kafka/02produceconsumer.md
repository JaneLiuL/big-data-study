![architect](images/04-kafkaarchitect01.png)

从数据可靠性来看，如果kafka broker其中一台挂了怎么办，所以kafka的设计就把.log放到其他broker server，如下图所示
但是kafka是没有备份的概念，kafka是称叫副本，多个副本中同时只有一个来进行读取操作，其他只是作为备份副本，具有读写能力的副本称为leader，其他备份的副本称为follower
![architect](images/05-kafka.png)

![architect](images/06-kafka.png)