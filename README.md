# xke

备份自己在 X 上点赞的帖子里的媒体：图片（原图）、视频（码率最高的 MP4）、GIF（X 提供的 MP4）。
只用 Go 标准库，没有外部依赖。

> 仅供自用。`backend/` 里除 `xke.go`、`xke_test.go` 外的文件来自
> [xdl](https://github.com/missuo/xdl)（GPL-3.0，提交号见 `backend/UPSTREAM`），

## 构建

```
go build -o xke .
go test ./...
```

## 使用

```
# 第一次：给出用户名，以及到 xx 帖子为止的 ID
xke sync --dir ~/xke-data --user <用户名> --until <帖子ID或链接> --until <备用ID>

# 之后（cron）
xke sync --dir ~/xke-data
```

- `fetch` 只抓清单，`download` 只按清单下载，`sync` 两步连跑。
- `--until`：翻到这条帖子就停，它本身和更早的点赞不下载。边界存进 `state.json`，以后每次都生效。
- `--all`：没有边界，抓整个点赞列表。第一次运行必须给 `--until` 或 `--all`。
- `--backfill`：一次性越过边界翻到底，比边界更早的帖子只下载视频和 GIF，图片跳过。
- `--limit N`：本次最多收 N 条帖子，达到就停并警告，之后不会自动接着往下抓。
- `--token`：`auth_token` 的值，或整段 Cookie。不给就读环境变量 `XKE_TOKEN`，再读 `<dir>/token`（请 `chmod 600`）。
- `--proxy`：代理地址。不给就不改任何网络设置（Go 默认会读 `HTTPS_PROXY` 等环境变量）。
- `--out`：媒体目录，默认 `<dir>/likes`。`--jobs`：并发数，默认 8。`--retries`：重试次数，默认 2。

之后的运行从最新的点赞往下翻，遇到边界、或者整页都是清单里已有的帖子就停。

## 数据目录

```
<dir>/
  likes/             媒体，不分子目录
  likes.jsonl        清单：每个媒体一行；没有媒体的帖子也占一行
  downloaded.jsonl   已下载记录
  state.json         用户名、边界、未完成抓取的游标
  token              auth token（可选）
```

- 文件名：`<发帖日期>_<作者>_<帖子ID>_p<序号>.<扩展名>`，日期按 Asia/Shanghai，序号是媒体在帖子里的位置，从 0 开始。
- 下载完成后，文件的修改时间设为发帖时间。
- 目录里已有同名文件时不覆盖，只记为已下载。
- 清单里 `skip` 字段说明某一行为什么不下载：`no-media`（帖子没有媒体）、`no-mp4`（视频只有流媒体、没有 MP4）、
  `no-url`、`before-boundary`（边界之前的图片）。
- 下载中断会留下 `.part` 文件，下次运行接着下。

## 跟进 xdl 的修复

X 的接口变了、xdl 修好之后，把 `xapi.go`、`xtxid.go`、`xtimeline.go`、`timeline.go`、`resumable.go`、
`netclient.go` 从 xdl 的 `backend/` 原样拷过来，再跑 `go test ./...`。
