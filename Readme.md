# 歌词 API 服务

腾讯音乐歌词代理和转换服务，支持 LRC、ESLRC、TTML 格式。

## 部署到 Vercel

1. 推送代码到 GitHub
2. 在 Vercel Dashboard 导入仓库
3. 自动部署完成

## API 使用

### 搜索歌曲
GET /v2/music/tencent/lyric?word=梦回还

### 获取歌词（通过 ID）
GET /v2/music/tencent/lyric?id=105648974

### 搜索并获取第 N 首
GET /v2/music/tencent/lyric?word=梦回还&n=1