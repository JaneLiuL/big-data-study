线程和线程的数据交互
![thread01](images/01thread.png)

进程和进程之间的数据交互
![process](images/02process.png)

但是如果数据在第一个进程发送速度每秒1000条，而进程二每秒消耗800条，会容易造成消息积压，
这个时候我们需要中间增加一个数据缓冲区来解耦，这个时候数据缓冲区就可以扮演数据的中转，我们也称为消息中间件


![kafka](images/03kafkawithapp.png)