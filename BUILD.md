# 编译说明

## 环境要求

- Go 1.24 或更高（`go.mod` 声明 `go 1.24.0`）
- 首次编译需联网拉取依赖，本机 GOPROXY 为 `https://goproxy.cn,direct`

确认环境：

```bash
go version
go env GOPATH GOPROXY
```

## 一、最常用：编译到发布目录

把源码改完后，用它替换发布目录里的可执行文件。

**Git Bash**

```bash
cd /d/devsoftware/vastbases/gomysql2pg-master/gomysql2pg-master

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" \
  -o "D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7/gomysql2pg.exe" .
```

**PowerShell**

```powershell
cd D:\devsoftware\vastbases\gomysql2pg-master\gomysql2pg-master

$env:CGO_ENABLED = "0"; $env:GOOS = "windows"; $env:GOARCH = "amd64"
go build -ldflags "-s -w" -o D:\devsoftware\vastbases\gomysql2pg-win-x64-v0.2.7\gomysql2pg.exe .
```

> PowerShell 没有 `VAR=value cmd` 这种行内环境变量语法，必须先 `$env:VAR = "..."`。
> 这些变量只对当前会话生效，用完可以 `Remove-Item Env:CGO_ENABLED` 清掉。

参数含义：

| 参数 | 作用 |
|---|---|
| `CGO_ENABLED=0` | 纯 Go 静态编译，不依赖系统 C 库 |
| `GOOS` / `GOARCH` | 目标平台，交叉编译时必填 |
| `-ldflags "-s -w"` | 去掉符号表和调试信息，二进制更小 |
| `-o` | 输出路径，可指向任意目录 |
| `.` | 编译当前目录（根包的 `main.go`） |

## 二、普通编译（编译到当前目录）

```bash
go build -o gomysql2pg.exe .
```

## 三、编译前自检

```bash
go build ./...                      # 编译全部包
go test -vet=off ./...              # 跑测试
```

> `-vet=off` 是必须的：`cmd/version.go:48` 和 `cmd/root.go:62` 存在两个既有的
> vet 告警（非常量格式串、无缓冲 signal channel），会让 `go test` 在编译阶段直接失败。
> 这两个问题与本项目功能无关，尚未修复。

## 四、验证编译结果

```bash
cd D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7
./gomysql2pg.exe version
```

正常输出当前版本号（如 `v0.3.1`）。

## 五、编译配套工具 xlsx2yml

从 Excel 批量生成 yml 配置的小工具，独立于主程序：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" \
  -o "D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7/xlsx2yml.exe" ./tools/xlsx2yml
```

## 六、全平台发布包

`Makefile` 的 `release` 目标会一次性打出 MacOS / linux-arm64 / linux-x64 / win-x64 四个包：

```bash
make release VERSION=v0.3.1
```

打包内容：`gomysql2pg` 主程序、`xlsx2yml`、`example.yml`、`check_log.*`、`run_batch.*`、`configs/example.xlsx`。

## 注意事项

### 版本号是硬编码的，改 Makefile 传参没用

`cmd/version.go:12` 里版本号写死：

```go
var ver = "v0.3.1"
```

`Makefile` 的 `build` 目标传的是 `-ldflags "-X main.Version=${VERSION}"`，但：

1. 变量名叫 `ver` 不是 `Version`
2. 变量在 `cmd` 包里不是 `main` 包

两个都对不上，所以 **Go 会静默忽略这个参数**，版本号始终是源码里的值。要真正支持注入，得把 Makefile 改成：

```makefile
go build -ldflags "-X gomysql2pg/cmd.ver=${VERSION}" -o ${BINARY} .
```

或者干脆直接改 `cmd/version.go` 里的字面量。

### 输出目录要和发布目录一致

程序的日志目录、`configs/` 都是**相对当前工作目录**解析的，不是相对 exe 位置。所以运行时要先 `cd` 到发布目录：

```bash
cd D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7
./gomysql2pg.exe --config configs/01_xxx.yml
```

### 依赖缓存位置

本机 GOPATH 是 `D:\devsoftware\go\cahces`（注意目录名拼写是 `cahces`），模块缓存在其下的 `pkg/mod`。依赖拉不下来时先检查 `go env GOPROXY`。
