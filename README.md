# isucon-template

## pprotein セットアップ

pprotein サーバは別途用意済みです。計測対象サーバと pprotein サーバで必要な作業を以下にまとめます。

### 計測対象サーバ（エージェント稼働サーバ）

```
cd /path/to/isucon-template
sudo mybin/pprotein/setup.sh install
```

- graphviz/gv（pprof グラフ描画用）と pprotein 本体/agent を取得し、systemd で `pprotein-agent` を起動します。`apt-get` が利用できない環境では graphviz/gv の導入はスキップされます。
- ログへのアクセス権は `setfacl` で付与します。無い場合は `sudo apt-get install acl` などで導入してください。
- nginx / MySQL のログパスが異なる場合は、環境変数 `PPROTEIN_HTTPLOG` / `PPROTEIN_SLOWLOG` を指定してください。バージョンやアーキテクチャも環境変数で上書き可能です。
- 後片付けは `sudo mybin/pprotein/setup.sh uninstall` で行えます。

#### MySQL のスロークエリログを有効化

```
cd /path/to/isucon-template
./mybin/mysql/mysql_slow_query.sh init   # 設定ファイルに slow_query_log エントリを追加
./mybin/mysql/mysql_slow_query.sh on     # slow_query_log = ON を適用
sudo systemctl restart mysql
```

- ログファイルはデフォルトで `/var/log/mysql/mysql-slow.log` を想定しています。別パスを使う場合は `mysql_slow_query.sh` と `setup.sh` の環境変数を合わせて変更してください。
- 計測終了後は `./mybin/mysql/mysql_slow_query.sh off` で元に戻してください。

#### Go アプリケーションへの pprotein ミドルウェア追加

- Web フレームワークに応じて `github.com/kaz/pprotein/pkg/middleware/...` を導入し、ハンドラにミドルウェアを差し込みます。例（Echo の場合）:

  ```go
  import (
    ppMiddleware "github.com/kaz/pprotein/pkg/middleware/echo"
  )

  e := echo.New()
  e.Use(ppMiddleware.New(ppMiddleware.Config{
    Endpoint: "http://127.0.0.1:19000",
  }))
  ```

- ルータに差し込むだけでリクエスト統計が pprotein-agent 経由で収集されます。詳細は公式ドキュメントを参照してください。

#### デプロイ／ベンチ後に webhook を叩くコード追加

- コード更新やベンチマーク完了に合わせて、pprotein サーバにスナップショット通知を送るための webhook を呼び出します。例:

  ```go
  import (
    "log"
    "net/http"
    "strings"
  )

  payload := strings.NewReader("{\"repository\":\"isucon\",\"branch\":\"main\"}")
  req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:19000/webhook", payload)
  if err != nil {
    log.Printf("webhook request build failed: %v", err)
    return
  }
  req.Header.Set("Content-Type", "application/json")
  resp, err := http.DefaultClient.Do(req)
  if err != nil {
    log.Printf("webhook call failed: %v", err)
    return
  }
  resp.Body.Close()
  ```

- webhook のパスや JSON フィールドは pprotein の設定に合わせて調整してください。

#### nginx のアクセスログを pprotein/alp 向けに整える

- `nginx` のアクセスログ書式を `alp` が解析しやすい LTSV 形式に揃えます。例として `/etc/nginx/nginx.conf` の `log_format` を以下に置き換えます:

  ```nginx
  log_format ltsv "time:$time_local"
                  "\thost:$remote_addr"
                  "\tforwardedfor:$http_x_forwarded_for"
                  "\treq:$request"
                  "\tstatus:$status"
                  "\tmethod:$request_method"
                  "\turi:$request_uri"
                  "\tsize:$body_bytes_sent"
                  "\treferer:$http_referer"
                  "\tua:$http_user_agent"
                  "\treqtime:$request_time"
                  "\tcache:$upstream_http_x_cache"
                  "\truntime:$upstream_http_x_runtime"
                  "\tapptime:$upstream_response_time"
                  "\tvhost:$host";

  access_log  /var/log/nginx/access.log ltsv;
  ```

- 設定変更後は `sudo nginx -t` で構文を確認し、`sudo systemctl reload nginx` で反映します。
- 既存ログを温存したい場合は別ファイルに退避してから設定を切り替えます。

### pprotein サーバ（可視化・解析サーバ）

- alp（アクセスログ解析）と slp（スロークエリ解析）は pprotein サーバで管理します。例:

  ```
  ALP_VERSION=1.0.21
  ALP_ARCH=linux_amd64
  curl -fsSL "https://github.com/tkuchiki/alp/releases/download/v${ALP_VERSION}/alp_${ALP_VERSION}_${ALP_ARCH}.tar.gz" -o /tmp/alp.tar.gz
  tar -xzf /tmp/alp.tar.gz -C /tmp
  sudo install -m 0755 /tmp/alp /usr/local/bin/alp

  SLP_VERSION=0.2.1
  SLP_ARCH=linux_amd64
  curl -fsSL "https://github.com/tkuchiki/slp/releases/download/v${SLP_VERSION}/slp_${SLP_VERSION}_${SLP_ARCH}.tar.gz" -o /tmp/slp.tar.gz
  tar -xzf /tmp/slp.tar.gz -C /tmp
  sudo install -m 0755 /tmp/slp /usr/local/bin/slp
  ```

  ※バージョンやアーキテクチャは適宜読み替えてください。不要になった `/tmp/*.tar.gz` や展開物も削除して構いません。
- pprotein UI の `setting > group/targets` に対象サーバの Private IP と `http://<target>:19000` を登録し、必要に応じて `httplog/config` も調整してください。
