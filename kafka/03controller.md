kafka controller选举过程
首先有三个broker, 他们就去尝试往zookeeper创建节点，谁先创建成功，谁就是集群的管理者，然后之后剩下的两个节点尝试创建，发现已经有节点了，就会建立监听，去监听节点的变化，一直等到集群管理者死了，然后zookeeper就删除该节点，然后监听的节点发现之后会马上尝试创建
![architect](images/09-kafka.png)