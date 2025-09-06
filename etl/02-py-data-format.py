import time
import threading
import queue
import re
from typing import Dict, List, Callable, Any, Optional
import json
import os
from watchdog.observersimport FileSystemEventHandler, Observer
import socket

class Event:
    """日志事件对象，类似Fluentd的Event"""
    def __init__(self, tag: str, timestamp: float, record: Dict[str, Any]):
        self.tag = tag
        self.timestamp = timestamp
        self.record = record

    def __repr__(self) -> str:
        return f"Event(tag={self.tag}, timestamp={self.timestamp}, record={self.record})"

class InputPlugin:
    """输入插件基类"""
    def __init__(self, tag: str, output_queue: queue.Queue):
        self.tag = tag
        self.output_queue = output_queue
        self.running = False
        self.thread: Optional[threading.Thread] = None

    def start(self):
        self.running = True
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def stop(self):
        self.running = False
        if self.thread:
            self.thread.join()

    def run(self):
        """子类需要实现此方法来产生事件"""
        raise NotImplementedError

class TailInput(InputPlugin, FileSystemEventHandler):
    """文件尾输入插件，类似fluentd的tail插件"""
    def __init__(self, tag: str, output_queue: queue.Queue, path: str, pos_file: str = "pos.json"):
        super().__init__(tag, output_queue)
        self.path = path
        self.pos_file = pos_file
        self.file_position = self._load_positions()
        self.observer = Observer()
        self.observer.schedule(self, path=os.path.dirname(path), recursive=False)

    def _load_positions(self) -> Dict[str, int]:
        """加载文件读取位置"""
        if os.path.exists(self.pos_file):
            with open(self.pos_file, 'r') as f:
                return json.load(f)
        return {self.path: 0}

    def _save_positions(self):
        """保存文件读取位置"""
        with open(self.pos_file, 'w') as f:
            json.dump(self.file_position, f)

    def on_modified(self, event):
        """文件修改时触发"""
        if not event.is_directory and event.src_path == self.path:
            self._read_new_content()

    def _read_new_content(self):
        """读取文件新内容"""
        try:
            with open(self.path, 'r') as f:
                pos = self.file_position.get(self.path, 0)
                f.seek(pos)
                while line := f.readline():
                    line = line.strip()
                    if line:
                        event = Event(
                            tag=self.tag,
                            timestamp=time.time(),
                            record={"message": line}
                        )
                        self.output_queue.put(event)
                new_pos = f.tell()
                if new_pos != pos:
                    self.file_position[self.path] = new_pos
                    self._save_positions()
        except Exception as e:
            print(f"Error reading file: {e}")

    def run(self):
        """启动文件监控"""
        print(f"Starting tail input for {self.path} with tag {self.tag}")
        self._read_new_content()  # 初始读取
        self.observer.start()
        while self.running:
            time.sleep(1)
        self.observer.stop()
        self.observer.join()

class TcpInput(InputPlugin):
    """TCP输入插件，接收网络日志"""
    def __init__(self, tag: str, output_queue: queue.Queue, host: str = "0.0.0.0", port: int = 24224):
        super().__init__(tag, output_queue)
        self.host = host
        self.port = port
        self.socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.socket.bind((host, port))
        self.socket.listen(5)
        self.socket.settimeout(1)  # 设置超时以便检查running状态

    def handle_client(self, client_socket):
        """处理客户端连接"""
        try:
            while self.running:
                data = client_socket.recv(1024)
                if not data:
                    break
                message = data.decode('utf-8').strip()
                if message:
                    event = Event(
                        tag=self.tag,
                        timestamp=time.time(),
                        record={"message": message}
                    )
                    self.output_queue.put(event)
        except Exception as e:
            print(f"Client handling error: {e}")
        finally:
            client_socket.close()

    def run(self):
        """启动TCP服务器"""
        print(f"Starting TCP input on {self.host}:{self.port} with tag {self.tag}")
        while self.running:
            try:
                client_socket, addr = self.socket.accept()
                print(f"Accepted connection from {addr}")
                client_thread = threading.Thread(target=self.handle_client, args=(client_socket,), daemon=True)
                client_thread.start()
            except socket.timeout:
                continue
            except Exception as e:
                print(f"TCP server error: {e}")
                break
        self.socket.close()

class FilterPlugin:
    """过滤插件基类"""
    def __init__(self, input_queue: queue.Queue, output_queue: queue.Queue, match_tags: List[str]):
        self.input_queue = input_queue
        self.output_queue = output_queue
        self.match_tags = match_tags
        self.running = False
        self.thread: Optional[threading.Thread] = None

    def start(self):
        self.running = True
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def stop(self):
        self.running = False
        if self.thread:
            self.thread.join()

    def matches(self, tag: str) -> bool:
        """检查事件标签是否匹配"""
        for pattern in self.match_tags:
            if pattern == tag or (pattern.endswith("*") and tag.startswith(pattern[:-1])):
                return True
        return False

    def filter(self, event: Event) -> Optional[Event]:
        """过滤处理事件，返回None表示过滤掉该事件"""
        raise NotImplementedError

    def run(self):
        """处理输入队列中的事件"""
        while self.running:
            try:
                event = self.input_queue.get(timeout=1)
                if self.matches(event.tag):
                    filtered_event = self.filter(event)
                    if filtered_event:
                        self.output_queue.put(filtered_event)
                else:
                    self.output_queue.put(event)  # 不匹配的事件直接传递
                self.input_queue.task_done()
            except queue.Empty:
                continue
            except Exception as e:
                print(f"Filter error: {e}")

class GrepFilter(FilterPlugin):
    """类似fluentd的grep过滤器，基于正则匹配保留或排除事件"""
    def __init__(self, input_queue: queue.Queue, output_queue: queue.Queue, match_tags: List[str],
                 key: str, pattern: str, exclude: bool = False):
        super().__init__(input_queue, output_queue, match_tags)
        self.key = key
        self.pattern = re.compile(pattern)
        self.exclude = exclude

    def filter(self, event: Event) -> Optional[Event]:
        """如果匹配模式则保留事件（或排除，取决于exclude参数）"""
        value = event.record.get(self.key, "")
        match = self.pattern.search(str(value)) is not None
        
        if (not self.exclude and match) or (self.exclude and not match):
            return event
        return None

class RecordTransformerFilter(FilterPlugin):
    """类似fluentd的record_transformer，用于修改事件记录"""
    def __init__(self, input_queue: queue.Queue, output_queue: queue.Queue, match_tags: List[str],
                 add_fields: Dict[str, Any] = None, remove_fields: List[str] = None):
        super().__init__(input_queue, output_queue, match_tags)
        self.add_fields = add_fields or {}
        self.remove_fields = remove_fields or []

    def filter(self, event: Event) -> Optional[Event]:
        """添加或移除事件记录中的字段"""
        # 添加字段
        for key, value in self.add_fields.items():
            event.record[key] = value
        
        # 移除字段
        for key in self.remove_fields:
            event.record.pop(key, None)
            
        return event

class OutputPlugin:
    """输出插件基类"""
    def __init__(self, input_queue: queue.Queue, match_tags: List[str]):
        self.input_queue = input_queue
        self.match_tags = match_tags
        self.running = False
        self.thread: Optional[threading.Thread] = None
        self.buffer: List[Event] = []
        self.buffer_size = 10  # 缓冲区大小
        self.flush_interval = 5  # 刷新间隔（秒）
        self.last_flush_time = time.time()

    def start(self):
        self.running = True
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def stop(self):
        self.running = False
        if self.thread:
            self.thread.join()
        self.flush()  # 停止时刷新缓冲区

    def matches(self, tag: str) -> bool:
        """检查事件标签是否匹配"""
        for pattern in self.match_tags:
            if pattern == tag or (pattern.endswith("*") and tag.startswith(pattern[:-1])):
                return True
        return False

    def emit(self, events: List[Event]):
        """输出事件，子类需要实现"""
        raise NotImplementedError

    def flush(self):
        """刷新缓冲区"""
        if self.buffer:
            try:
                self.emit(self.buffer)
                self.buffer = []
                self.last_flush_time = time.time()
            except Exception as e:
                print(f"Flush error: {e}")

    def run(self):
        """处理输入队列中的事件"""
        while self.running:
            try:
                # 检查是否需要刷新缓冲区
                current_time = time.time()
                if len(self.buffer) >= self.buffer_size or current_time - self.last_flush_time >= self.flush_interval:
                    self.flush()

                event = self.input_queue.get(timeout=1)
                if self.matches(event.tag):
                    self.buffer.append(event)
                self.input_queue.task_done()
            except queue.Empty:
                continue
            except Exception as e:
                print(f"Output error: {e}")
        
        # 确保最后刷新一次
        self.flush()

class StdoutOutput(OutputPlugin):
    """输出到标准输出"""
    def emit(self, events: List[Event]):
        for event in events:
            print(f"[{time.ctime(event.timestamp)}] {event.tag}: {event.record}")

class FileOutput(OutputPlugin):
    """输出到文件"""
    def __init__(self, input_queue: queue.Queue, match_tags: List[str], path: str):
        super().__init__(input_queue, match_tags)
        self.path = path

    def emit(self, events: List[Event]):
        with open(self.path, 'a') as f:
            for event in events:
                line = json.dumps({
                    "tag": event.tag,
                    "timestamp": event.timestamp,
                    "record": event.record
                }) + "\n"
                f.write(line)

class FluentdLite:
    """简化版Fluentd主类"""
    def __init__(self):
        self.queues = []
        self.inputs: List[InputPlugin] = []
        self.filters: List[FilterPlugin] = []
        self.outputs: List[OutputPlugin] = []

    def create_queue(self) -> queue.Queue:
        """创建一个新的队列"""
        q = queue.Queue()
        self.queues.append(q)
        return q

    def add_input(self, input_plugin: InputPlugin):
        """添加输入插件"""
        self.inputs.append(input_plugin)

    def add_filter(self, filter_plugin: FilterPlugin):
        """添加过滤插件"""
        self.filters.append(filter_plugin)

    def add_output(self, output_plugin: OutputPlugin):
        """添加输出插件"""
        self.outputs.append(output_plugin)

    def start(self):
        """启动所有组件"""
        print("Starting Fluentd Lite...")
        for output in self.outputs:
            output.start()
        for filter_plugin in self.filters:
            filter_plugin.start()
        for input_plugin in self.inputs:
            input_plugin.start()

    def stop(self):
        """停止所有组件"""
        print("Stopping Fluentd Lite...")
        for input_plugin in self.inputs:
            input_plugin.stop()
        for filter_plugin in self.filters:
            filter_plugin.stop()
        for output in self.outputs:
            output.stop()

if __name__ == "__main__":
    # 创建一个简化版Fluentd实例
    fluent = FluentdLite()

    # 创建队列连接各个组件
    input_queue = fluent.create_queue()
    filter_queue = fluent.create_queue()
    output_queue = fluent.create_queue()

    # 添加输入：监控文件和TCP端口
    file_input = TailInput(
        tag="app.log",
        output_queue=input_queue,
        path="/var/log/app.log",
        pos_file="app_log.pos"
    )
    
    tcp_input = TcpInput(
        tag="network.log",
        output_queue=input_queue,
        port=24224
    )
    
    fluent.add_input(file_input)
    fluent.add_input(tcp_input)

    # 添加过滤：只保留包含"error"的日志，并添加环境字段
    grep_filter = GrepFilter(
        input_queue=input_queue,
        output_queue=filter_queue,
        match_tags=["app.log", "network.log"],
        key="message",
        pattern=r"error",
        exclude=False
    )
    
    transformer_filter = RecordTransformerFilter(
        input_queue=filter_queue,
        output_queue=output_queue,
        match_tags=["app.log", "network.log"],
        add_fields={"environment": "production", "source": "fluentd-lite"}
    )
    
    fluent.add_filter(grep_filter)
    fluent.add_filter(transformer_filter)

    # 添加输出：控制台和文件
    stdout_output = StdoutOutput(
        input_queue=output_queue,
        match_tags=["app.log", "network.log"]
    )
    
    file_output = FileOutput(
        input_queue=output_queue,
        match_tags=["app.log", "network.log"],
        path="/var/log/filtered_errors.log"
    )
    
    fluent.add_output(stdout_output)
    fluent.add_output(file_output)

    # 启动服务
    try:
        fluent.start()
        print("Fluentd Lite is running. Press Ctrl+C to stop.")
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        fluent.stop()
        print("Fluentd Lite stopped.")
