# TigerNas 長大チャット切り分け runbook

## 境界

ホストの `mcp_http` に相関 ID がなければ、MCPX、Remote Session、Codex の障害とは判定しない。ChatGPT がそのメッセージで App を選択できなかった事象として `client_request_not_received` / `app_not_selected_or_unavailable` を調べる。相関 ID があれば、同じ `request_id` の `mcp_tool` outcome、HTTP status、payload bytes、schema revisionで host 到達後の原因を判定する。

ログには prompt、command、source、stdout/stderr、Authorization、token、PIIを残さない。principal と MCP transport session は hash のみとする。OAuth refresh と tunnel reconnect は MCPX が観測できないため `not_observed_by_mcpx` と記録し、tunnel/client側の同時刻ログで補う。

| evidence | classification |
| --- | --- |
| host requestなし | `client_request_not_received` / `app_not_selected_or_unavailable` |
| 401 | `oauth_access_expired`。client側refresh結果を確認 |
| schema validation / frozen tool error | `tool_schema_validation_failed` / `tool_snapshot_mismatch` |
| 413またはresponse budget超過 | `response_too_large` |
| tunnel timeout/reconnect | `tunnel_timeout` / `transport_unhealthy` |
| session `not_found` | `remote_session_not_found` |
| Task stopped / failed / resultなし | `codex_task_stopped` / `codex_task_failed` / `result_missing` |
| request + tool resultあり | `success`（business tool errorは別の `status` で確認） |

## Test A-E

1. **A — client選択**: 新規ChatGPTテキストチャットでTigerNasを選択または `@TigerNas` と明示し、`workspace` を1回呼ぶ。JST時刻とhost `request_id` を控える。requestがなければhost障害としない。
2. **B — compact resume**: 同じチャットで既知の `remote_session_id` を `session(action=open)` へ原文のまま渡す。`response_profile=compact`、response bytes、workspace、git headを確認する。
3. **C — 長大chat比較**: 問題の長大チャットで再度Appを `@mention` し、同じ時間窓のhost requestを確認する。requestなしはclient/app/model/surface側、401/403はauth側、schema errorはsnapshot側、timeout/413はpayload/tunnel側。
4. **D — cross-chat resume**: 新規チャットで同じIDをopenし、workspace、git head、active Task ID、artifact countが一致することを確認する。IDを失った場合は `session(action=list, workspace, query, status)` のworkspace、label、status、`last_active_at` で再発見する。同名候補が複数なら選択を止める。
5. **E — frozen snapshot**: host schemaを変えていない状態でAppを再選択する。復旧しなければSettings → AppsでTigerNasをRefreshする。未公開のDeveloper Mode appはScan Tools後に接続する。host schema変更だけを反映完了とはしない。

## Compact / full / logs

- `session` はdefault compact。完全inventoryが必要な時だけ `response_profile=full` を明示する。
- 前回の `revisions` を `known_revisions` に渡し、`changed_sections` が空なら前回factsを再利用する。
- `execute(action=attach)` / `observe(view=logs)` は返却されたstdout/stderr offsetを次回へそのまま渡す。command本文は再送されず、`command_digest` と短いpurposeだけが返る。
- 続きは `resource_uri` を読む。durable task logは0600、最大10 MiBで、それ以降はbounded truncationとなる。

## 文脈非依存 handoff

ChatGPT会話履歴を正本にしない。作業開始時に次をworkspace上のcheckpointへ書き、`artifact(action=register)` でRemote Sessionへ登録する。実在するCodex session IDを取得できた時だけ `codex_resume_supported=true` とする。IDがなければGit、checkpoint、PR、diff、summaryから新しいCodexへhandoffする。

```json
{
  "workspace": "",
  "remote_session_id": "",
  "label": "",
  "branch": "",
  "worktree": "",
  "git_head": "",
  "active_execution_task_ids": [],
  "codex_session_id": "",
  "codex_resume_supported": false,
  "last_completed_checkpoint": "",
  "next_action": "",
  "human_gate": ""
}
```

## OAuth / schema / Human Gates

OAuth使用時だけdiscovery、`offline_access`、refresh発行・rotation、期限切れ→refresh、平文非保存をfixtureで確認する。metadata変更後のApp再認証はH3。現行profileがOAuthを広告しない場合、OAuth原因と推測しない。

tool schema revisionはregistered toolのschemaだけから安定生成する。比較ではtool名変更、field削除/rename/型変更、required追加をbreaking、optional field追加だけをcompatibleとする。本変更は`session.response_profile`と`session.known_revisions`のoptional追加のみ。host反映はH1、service restartはH2、ChatGPTのRefresh/Scan ToolsはH4、breaking変更はH5で停止する。
