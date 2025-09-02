
Dockerfile（x86_64 Ubuntu + Rust + eBPF 工具链）见./dockerfile
构建并运行：
```bash
# 构建镜像
docker build -t rust-ebpf-dev:0.0.1 .

# 运行容器（挂载本地代码目录）
docker run -it --privileged \
  -v $(pwd):/workspace \
  rust-ebpf-dev:0.0.1
```

然后我们可以使用如下命令在mac上开发
```bash
docker run -it --privileged --name ebpf-dev03 --platform linux/amd64  -v /Users/jane/workspace/ebpf:/workspace -v /Users/jane/workspace/ubunturoot:/root janeliul/rust-develop:0.0.2
```


docker update --memory=4g --memory-swap=8g
rustup show
切换到全局使用nightly，而不使用stable
rustup default nightly
进入ebpf构建

cargo build -Zbuild-std

cargo build -Zbuild-std



