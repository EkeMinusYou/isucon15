# ISUReal 外部仕様・提供データ

## この文書の位置づけ

2026-10-31に提供されたISUCON2026マニュアル、稼働中の参照アプリケーション、提供初期データ、参加者向けポータルのHARを照合した設計入力。リポジトリ名とHAR名はisucon15だが、対象サービスはISURealである。

- **公式**：当日マニュアルの明示要件。特に「通知エンドポイント」「競技環境」「変更してはいけない点」「負荷走行」「画像URLの検証」「データの反映」「スコアの計算」「追試」。提供マニュアルの該当章題を根拠とする。元マニュアルには旧構成の説明もあるため、bootstrapの入力は本書に抽出した外部要件とし、元資料の内部説明を引き継がない。
- **参照動作**：初期実装の通常経路・分岐を照合して得た外部動作。以下で断りのないAPI詳細はこの区分。内部実装方法を要求するものではない。
- **推論**：公式要件から必要となる条件。根拠と不確実性を明記する。
- **観測**：公開HTTP応答、実ホスト資源、HARまたは実ポータル応答で確認した事実。

全42個のmethod/path組を照合した。APIの通常経路と状態変更の関連はソースで確認したが、全入力・全競合の実HTTP試験はしていない。新構成の実装・初期化・復元・ベンチPASSは未検証で、別セッションのbootstrapが行う。旧ソース、内部設定、旧データ定義、調査スクリプトはリポジトリ外に分離してあり、bootstrapの入力にしない。

## 共通のHTTP契約

- API prefixは`/api/`。JSONの属性名はcamelCase。ただし投稿multipartの位置情報は後述のsnake_case。成功で本文を返さない操作は204。
- エラーは原則`{"error":"コード"}`。以下の`400 invalid_post`等はHTTP statusとerrorの値。JSON・multipart・時刻の構文不正は400系のbinding error。具体的なframework由来文字列は本書で固定しない。
- 文字数制限は妥当なUTF-8のUnicodeコードポイント数。usernameのみ`[a-z0-9_.-]{1,64}`。大文字化・空白除去を暗黙に行わない。
- IDは文字列。新規IDと認証用の値は参照動作ではUUID。公開された既存IDは提供値をそのまま維持する。日時はRFC3339系の時差付き文字列、小数秒を保持する。日付は`YYYY-MM-DD`、月は`YYYY-MM`。
- 一覧は原則`{items:[],hasMore:false}`、1ページ50件。51件目があればhasMore=true。カーソルは排他的。指定された対の片方だけが空なら`400 invalid_pagination`。空配列をnullにしない。
- Cookie認証は使わず`Authorization: Bearer <token>`。一般トークン不正・欠落・無効化・退会は`401 invalid_session`。管理者は別トークンで`401 invalid_administrator_session`。
- 一般ログインは同一ユーザーの既存トークンをすべて無効にする。ログアウトは提示トークンだけを無効にする。管理者ログインは既存管理者トークンを無効化しない。
- 通常の認証APIはアクセス先サーバーの固定regionと現在のユーザーregionが違うと`403 region_mismatch`。例外は一般ログイン・登録、ログアウト、PATCH /me、PUT/DELETE /me/profile-image。管理者API・画像・initializeにもこのregion検査はない。
- 書き込み成功後の読み取りに結果を反映し、リージョンをまたいでも同じである（公式）。画像削除の5秒猶予を、認証、関係、公開範囲、投稿一覧に適用してはいけない。
- 参照動作にIdempotency-Keyや条件付き更新の契約はない。重複時の結果は各APIに従う。

## 応答の共通型

| 型 | 属性 |
|---|---|
| SessionUser | id, username, displayName, bio, region, accountType (`regular`/`certified_public`), status (`active`/`deleted`) |
| Me | SessionUserにprofileImageUrl（文字列またはnull）を追加 |
| PublicUser | id, username, displayName, profileImageUrl（null可）, bio, accountType。region/statusは含めない |
| Window | id, region, localDate, startsAt, onTimeDeadline |
| Location | kind=`none`ならkindのみ。`exact`ならlatitude,longitude。`approximate`ならlocality |
| Post | id, author:PublicUser, postingWindowId, imageUrl, caption, visibility, location, postedAt, onTime, lateSeconds |
| Comment | id, author:PublicUser, body, createdAt |
| RealMoji | id, reactor:PublicUser, reactionType, imageUrl, createdAt |
| Relationship | relationId, user:PublicUser, status, updatedAt |
| Memory | Postにrealmojis:RealMoji[], comments:Comment[]を追加 |

投稿や交流の利用者情報は応答時点の現在のプロフィールを使う。Postのauthor、Commentのauthor、RealMojiのreactorのbioは、参照動作では格納値を返す。プロフィール単体・ユーザー検索・関係一覧のbio秘匿とは別の規則である。

## アカウント・プロフィール

| Method / path | 入力・成功 | 受付条件・エラー・更新効果 |
|---|---|---|
| POST /api/auth/register | JSON username, displayName, region。201 `{secretKey,user:SessionUser}` | displayName 1–100文字、regionはasia/europe/america。400 invalid_registration。usernameは退会済みを含め一意、409 username_taken。初期bioは空、regular/active、画像なし。トークンは発行しない |
| POST /api/auth/login | JSON username, secretKey。200 `{token,user:SessionUser}` | username不正、不存在、秘密鍵違い、退会済みは401 invalid_credentials。保存した秘密鍵とのSHA-256比較。成功時に前の一般セッションを置き換える |
| POST /api/auth/logout | 204 | 認証必須。提示したトークンを無効化 |
| GET /api/me | 200 Me | 通常の認証・region検査。画像URLは現在の画像に対する署名付きURL |
| PATCH /api/me | JSON displayName?, bio?, region?。200 Me | 少なくとも1項目。displayName 1–100、bio 0–500、regionは3値。400 invalid_profile。nullは未指定相当。region変更後は既存トークンで新regionの入口を使う。旧regionのSSEは終了する |
| PUT /api/me/profile-image | 本文JPEG、Content-Typeが厳密に`image/jpeg`。204 | 415 unsupported_media_type。JPEGでない、幅/高さ>512は400 invalid_profile_image。置換前の画像を無効にする。不正JPEGはIP制限の対象（大きさ超過とは区別） |
| DELETE /api/me/profile-image | 204 | 画像なしは404 account_not_found。成功後はnull、旧URLは無効 |
| POST /api/me/account-deletion | 204 | 通常認証・region検査。退会状態にして一般セッションを全無効化。以降ログイン不能。投稿/画像/交流の取得から退会者を除く。usernameは再利用できない |
| GET /api/users?q=... | 200 `{items:PublicUser[],hasMore}` | q 1–100文字、不正は400 invalid_relationship。自己・退会者・双方向どちらかがブロックした相手を除外。usernameまたはdisplayNameの部分一致、NFC→Unicode case-fold→NFCで比較。username, id昇順。beforeUsername/beforeIdは名称がbeforeでも**その組より大きい**もの。regularのbioは友達でも空文字 |
| GET /api/users/:userId | 200 PublicUser | 不存在・退会・双方向ブロックは404 relationship_not_found。certified_publicまたは友達ならbioを返し、それ以外は空文字。自己でも友達判定がないためregularのbioは空になる参照動作 |

## 投稿期間・投稿の受付

管理者がregionごとに単一の投稿期間を開始する。次の期間の開始で直前の期間が終了する（公式）。localDateは投稿を分類する業務日付であり、期限はonTimeDeadlineで判定する。

| Method / path | 入力・成功 | 受付条件・エラー |
|---|---|---|
| POST /api/admin/auth/login | JSON username,password。200 `{token,administratorId}` | username形式不正・認証不一致は401 invalid_credentials |
| POST /api/admin/auth/logout | 204 | 管理者認証。提示トークンだけ無効化 |
| PUT /api/admin/users/:userId/certification | 204 | 管理者認証。activeなregularだけcertified_publicへ変更。不存在・退会・既に認定済みは409 certification_conflict |
| DELETE /api/admin/users/:userId/certification | 常に409 certification_irreversible | 管理者認証。認定解除はできない |
| POST /api/admin/posting-window | JSON region,localDate,onTimeSeconds?。201 Window | region/date不正400 invalid_posting_window。同regionにlocalDate以上の既存期間があれば409 posting_window_conflict。期間の日付は単調増加。省略時86400秒、参照動作では0/負の秒も明示制限しない |
| GET /api/posting-window/current | 200 `{window:Window|null,hasPosted,canPost,deletionCount}` | 現在の自分のregionの期間がなければnull,false,false,0 |
| POST /api/posts | multipart image,caption,visibility,location_kind,latitude?,longitude?,locality?。201 Post | 後述の条件。400 invalid_post、409 post_slot_unavailable |
| GET /api/posts/me/current | 200 Post | 現在のregionの期間と**同じlocalDate**の削除していない自己投稿。なければ404 post_not_found |
| GET /api/posts/:postId | 200 Post | 不存在、削除済み、退会した投稿者、閲覧権なしは404 post_not_found |
| PATCH /api/posts/:postId | JSON caption?,visibility?。200 Post | 少なくとも1項目、caption 0–1000文字、visibilityは3値。400 invalid_post。不存在/削除404 post_not_found。他人の投稿、投稿期間終了済みは403 post_not_editable |
| DELETE /api/posts/:postId | 204 | 不存在/削除済み404 post_not_found。他人の投稿403 post_not_editable。そのlocalDateで既に2件削除済みなら409 post_slot_unavailable。**期間終了後も削除可** |

投稿には画像が必須。JPEGとしてdecode可能で幅・高さとも4096以下。captionは0–1000文字。visibilityは`friends` / `friends_of_friends` / `public`。
位置は`none`（値なし）、`exact`（latitude [-90,90]、longitude [-180,180]、localityなし）、`approximate`（locality 1–100文字、緯度経度なし）。他の組合せは400 invalid_post。

受付判定はユーザーとlocalDate単位。regionを変更しても同じ日付の投稿・削除を合算する。現在のregionに未終了の投稿期間があり、startsAt以降、同日付に削除していない投稿が0、同日付の削除数が2未満のときだけ投稿できる。GET currentのcanPostも同じ条件である。1回削除した後は再投稿可、2回削除後は不可。期間の終了は削除数をリセットせず、新しいlocalDateへの移行で集計対象が変わる。

postedAtはアップロード受付処理中に確定したサーバー時刻。onTimeはpostedAt <= onTimeDeadline、lateSecondsはmax(0,floor((postedAt-onTimeDeadline)/1秒))。期限後も期間終了までは受付可能。境界時の「画像送信開始時刻」では判定しない。

期間開始の成功時には、同じregionのその時点でactiveな全ユーザーへの通知が存在する。開始後の新規登録やregion転入へ遡って通知は作らない。前の期間の終了と新期間・通知の公開を整合させる必要がある（公式の成功後可視性からの推論）。登録・期間開始・投稿・region移動が同時の場合の全順序を実HTTPで確定してはいない。

## 閲覧権限とフィード

削除されていない投稿とactiveな投稿者が前提。自己投稿は期間終了後も詳細取得可能。他人の投稿には以下が**すべて**必要。

1. 相手の投稿期間が未終了。
2. 閲覧者が、現在所属するregionの現在の期間と同じlocalDateに、削除していない自分の投稿を持つ。投稿先regionや投稿期間IDの一致ではない。
3. 相互どちら向きにもブロックがない。
4. 公開範囲がpublic、またはfriendsかつ直接の友達、またはfriends_of_friendsかつ直接の友達/友達を1人介した関係。

従って自分の投稿を削除すると他人の投稿を読む権利も失う。region移動で現在の日付が変わると同様に変わる。相手の期間終了は相手の投稿を非公開にするが、自己のMemories/詳細まで消さない。公開範囲、ブロック、友達関係、退会は後続の照会・更新判定へ反映する。

`GET /api/feeds/:feed`は200 `{items:Post[],hasMore}`。自分の投稿を含めず、上記閲覧権限に加えて次の対象条件を適用する。

| feed | 対象・順序 |
|---|---|
| friends | 直接の友達。postedAt DESC,id DESC |
| following | 自分がフォローした相手。postedAt DESC,id DESC |
| public | visibility=public。postedAt DESC,id DESC |
| friends-of-friends | 友達を介した相手だが直接の友達ではない。score DESC,postedAt DESC,id DESC。Postにscoreを追加 |

不正なfeedは400 invalid_post。beforePostedAt/beforeIdを対で指定し、その組より小さいもの。friends-of-friendsはさらにbeforeScoreが必要で、(score,postedAt,id)の辞書式で小さいもの。

friends-of-friendsのscoreは次の整数値（参照動作）。`M(x)`を閲覧者とxの共通の友達数、`E(post)`をその投稿に現在有効なRealMojiまたは未削除コメントを持つ閲覧者の直接の友達の**重複しない人数**とする。退会した反応者・コメント者はEに含めない。`C(author)=Σ M(x)`（投稿者の直接の友達xについての和）。score=`10*E + 3*M(author) + min(C(author),30)`。同じ友達が複数コメント・RealMojiの両方を持ってもEは1。置換・削除・退会・友達変更は次の照会の順位を変える。M/Cの関係グラフでは、参照動作は退会・ブロックの追加フィルターを掛けない（直接ブロック時に関係削除は行う）。

## 友達・フォロー・ブロック

関係変更の共通条件は相手IDが非空・自分以外（400 invalid_relationship）、双方active（404 relationship_not_found）。友達とフォローは独立で、regionをまたいで成立する。

| Method / path | 成功 | 受付と状態遷移 |
|---|---|---|
| POST /api/friend-requests | JSON recipientId。201 `{relationId,status,updatedAt}` | ブロック中/既に友達/同方向pendingありは409 relationship_conflict。逆方向pendingがあれば既存申請をacceptedにして友達成立、新規申請は作らない。それ以外はpendingを作る |
| POST /api/friend-requests/:requestId/:action | 200 `{relationId,status,updatedAt}` | action=accept/rejectは受信者、cancelは送信者。申請不存在404 relationship_not_found。action不正、権限なし、pending以外は403 relationship_action_forbidden。acceptでブロック中なら409 relationship_conflict。acceptは友達成立、rejectはrejected、cancelはcancelled |
| GET /api/relationships/:kind | 200 `{items:Relationship[],hasMore}` | kind=friends/following/followers/incoming-requests/outgoing-requests。それ以外400 invalid_relationship。申請一覧はpendingのみ。相手がactiveのみ。statusは関係一覧ならkind、申請一覧ならpending。updatedAt DESC,relationId DESC、beforeUpdatedAt/beforeIdで排他ページング |
| DELETE /api/friendships/:userId | 204 | 友達関係を削除。不成立なら409 relationship_conflict。元のaccepted申請は変更しない |
| PUT /api/follows/:userId | 204 | 相手がcertified_publicかつブロックなし。既にフォロー、不認定、ブロック中は409 relationship_conflict |
| DELETE /api/follows/:userId | 204 | 自分からのフォローだけ削除。なければ409 relationship_conflict |
| PUT /api/blocks/:userId | 204 | 同方向ブロックがあれば409 relationship_conflict。友達関係、双方のフォローを削除し、双方のpending申請をcancelledにする。逆方向ブロックが既にあっても自分からの追加は可能 |
| DELETE /api/blocks/:userId | 204 | 自分が作ったブロックだけ削除。なければ403 relationship_action_forbidden。友達・フォロー・申請は復元しない |

関係一覧でregularのbioを返すのはfriendsのみ。certified_publicならすべてで返す。検索のbio規則とは区別する。

同時の逆方向申請は最終的に二重の友達関係を作らない参照動作。承認・ブロック・解除などの重複更新は上記条件で判定し、後続GETに成功後の状態を反映する。全競合組合せのHTTP試験は未実施。

## コメント・RealMoji

**コメント権限**は閲覧権限より狭い。対象投稿が未終了の期間内で、閲覧可能であり、自己投稿なら自分の現在の日付への投稿済み条件も必要。他人のpublic投稿なら可。他人のfriends/friends_of_friends投稿は**直接の友達だけ**が可。友達の友達で読むことができてもコメントできるとは限らない。コメントの一覧にも同じコメント権限を適用する。

| Method / path | 入力・成功 | 受付・更新 |
|---|---|---|
| POST /api/posts/:postId/comments | JSON body 1–1000文字。201 Comment | 文字不正400 invalid_interaction。投稿不存在/権限不足403 interaction_forbidden。重複文も別コメント |
| GET /api/posts/:postId/comments | 200 `{items:Comment[],hasMore}` | コメント権限がなければ403 interaction_forbidden。未削除かつ作者active。createdAt,id ASC。afterCreatedAt/afterIdより大きいもの |
| DELETE /api/comments/:commentId | 204 | コメント作者または投稿作者のみ。不存在/既に削除404 interaction_not_found。他人403 interaction_forbidden。投稿期間・閲覧権を再検査しない |
| POST /api/posts/:postId/realmojis | multipart image,reactionType。201 RealMoji | JPEGで幅/高さ<=512。reactionType=like/smile/surprise/heart/laugh/instant。入力不正400 invalid_realmoji。閲覧不可・不存在・自己投稿403 interaction_forbidden。1投稿×1反応者につき有効なものは1件。前の反応を置換して新IDを返す |
| GET /api/posts/:postId/realmojis | 200 `{items:RealMoji[],hasMore}` | 閲覧権限なし403 interaction_forbidden。自己の終了済み投稿でも閲覧可。未置換/未削除かつ反応者active。createdAt,id DESC。beforeCreatedAt/beforeIdより小さいもの |
| DELETE /api/realmojis/:realMojiId | 204 | 反応者または投稿作者のみ。不存在/既に削除404 interaction_not_found、他人403 interaction_forbidden。置換済みIDでも未削除なら削除可。投稿期間・閲覧権を再検査しない |

反応置換時に旧画像は無効になる。コメント/反応を消すと一覧・Memoriesとfriends-of-friendsのEへ反映される。退会者の交流は一覧から除外される。自己投稿へのRealMojiは禁止だが自己コメントは上の条件で可能。

## Memories・エクスポート

- `GET /api/memories?month=YYYY-MM`：200 `{items:Memory[],hasMore}`。不正な月は400 invalid_memory_period。
- `GET /api/memories/:date`：同じ型。date=YYYY-MM-DD。不正は400 invalid_memory_period。
- 自己の削除していない投稿のみ。対象月/日は**投稿期間のlocalDate**によって選び、postedAtのUTC日付ではない。postedAt DESC,id DESC、beforePostedAt/beforeIdで排他的ページング。
- 各投稿に有効なRealMoji（createdAt DESC,id DESC、先頭50件）と未削除コメント（createdAt ASC,id ASC、先頭50件）。交流作者のactive条件は通常一覧と同じ。終了した投稿の履歴でもこの内包一覧は返す。
- `GET /api/me/export`：200 application/zip、Content-Disposition: `attachment; filename="isureal-posts.zip"`、Cache-Control: no-store。自己の削除していない**全期間**の投稿をpostedAt ASC,id ASCで返す。`posts.json`はPostにimagePathを加えた配列。画像は`images/000001.jpg`から連番、原本のバイト列。参照ZIPでは画像entryはStore、posts.jsonは通常ZIP圧縮。交流履歴はエクスポートに含まない。
- 終了した投稿、古いプロフィール画像・交流の現在の有効性は日付を理由に省略しない。初期履歴と画像が期待値に一致することは公式のクリティカルチェック。

## 署名付き画像

`GET /api/images/:imageId?exp=<integer>&sig=<base64url>`は一般認証不要。全応答でCache-Control: no-store。成功は200、元のmediaTypeと元のバイト列。失敗は403 image_forbidden。

- exp/sigは各1個だけ。expはint64の10進正規表現で、解析後の再文字列化と完全一致（+、不要な先頭0など不可）。exp <= 現在のUnix秒なら失効。
- sigはpaddingなしの正規なURL-safe Base64、decode後32byte。余分なpaddingや非正規表現を拒否。
- 署名鍵は画像所有者のsecretKeyのSHA-256（提供accountsのsecretKeySha256をhex decodeした32byte）。初期メッセージは`decimal(exp) + ":" + imageId`。この値にHMAC-SHA256を適用し、直前の32byte出力へ同じ鍵でHMACを繰り返して**合計1000回**。最終値をRaw URL-safe Base64にする。
- APIが発行するURLのexpは発行時刻+24時間のUnix秒。既存に発行されたURLを含め、署名方式と検証結果を変えない（公式）。
- 所有者active、かつ現在のプロフィール画像、未削除の所有者の投稿画像、有効な未置換/未削除RealMojiの画像（その親投稿は未削除で作者active）のいずれかに該当すると有効。
- 画像URL自体に閲覧者の認証・友達関係は入らない。期間終了、公開範囲変更、ブロックは署名済み画像の参照動作では再検査しない。ただし投稿詳細から権限のない画像URLを発行してはならない。
- 投稿削除後5秒を過ぎたら有効期限内URLでも取得不可（公式）。参照動作は即時無効。プロフィール置換/削除、反応置換/削除、所有者退会も参照動作では即時無効。

## SSEとIP制限

`GET /api/notifications`は通常認証・region検査、200 text/event-stream。接続受付時刻より**後**に作られ、その時点でまだ未終了の投稿期間の自己通知をcreatedAt,id ASCで1件返して終了する。形式は`data: {"id":...,"postingWindowId":...,"region":...,"createdAt":...}\n\n`。最初にheaderをflushし、参照動作の確認間隔は500ms。通知がなければ接続を待機し、ユーザー退会・region変更・client disconnectで終了する。Last-Event-IDによる過去通知再送、定期heartbeat、複数イベントの継続送信は参照動作にない。接続済みSSEの待機中は一般トークンのlogout/replacedを再検査せず、ユーザーのactive/regionだけを再検査する参照動作。通知のcreatedAtは期間開始時刻と同じであり、配信時刻とは限らない。期間終了済みの通知は後から配信しない。

アプリでの接続元IPはX-Client-IPの値を使用し、省略時は直接接続IP。ルーターがX-Client-IP省略時に値を補う（公式）。アプリの参照動作は非空の値を追加検証しない。

次のIP単位の失敗を種類別に数え、BANは2分。既存BANより遅い解除時刻を残す。BAN中はAPI入口で403 ip_banned。禁止状態の反映には参照動作で最大約200msの更新間隔がある。静的ファイルはこのアプリ側判定を経由しない。

| 種類 | 対象となる結果 | 閾値 |
|---|---|---|
| 一般ログイン失敗 | POST /auth/loginの401 | 10秒未満の範囲内で10件 |
| 不正プロフィールJPEG | PUT /me/profile-image、decode不能による400 | 5秒未満の範囲内で2件 |
| 不正画像署名 | 有効な書式・有効期限・存在するactive所有者まで到達し、HMAC不一致による403 | 5秒未満の範囲内で3件 |

媒体型違い、サイズ超過、期限切れ、書式不正、画像不存在は上の署名/画像失敗とは同一に扱わない。参照の検出はホストごとの入力に基づき、成立したBANは全regionで参照される。BAN成立時にはその種類のそのIPの失敗カウントを空にする。全region横断の失敗数の合算や再起動をまたぐ未到達カウンターの保持までは公式に明記されていない。初期化後のBANは0件。ログに依存した旧方式そのものは設計要求ではなく、外部の拒否動作を維持する。

## 初期化・永続性

`POST /api/initialize`は認証不要、成功200 `{"language":"go","agent":"...","harness":"...","model":"..."}`。languageは空にしない。後3項目は実際に使用したagent、実行ツール、モデルを申告する（公式）。全regionで初期状態へ戻し、初期履歴・画像も取得可能にする。再設計が加えた派生状態も初期化対象。

本書の提供データが初期状態の再現入力である。一般/管理者セッション、BANは空。アカウント情報、関係、投稿期間、投稿、交流、通知と全画像は提供値から復元する。初期化時間の具体的な上限はマニュアルでは数値未記載。最終チェックの制限内に完了が必要で、bootstrapで実測確認する。

通常のサービス・OS再起動後も受理済みデータが取得できることは公式要件。追試は全4台とルーターの再起動、再ベンチ、その後の再起動と書込データ取得・ブラウザ表示を検査する。電源断/強制リセット耐性はこのプロジェクトの要件に含めない。永続データは配布で上書きされる場所の外へ置く。

## 配置・資源・接続条件

以下は観測値。CPUは全台2 vCPU、メモリ約4GB、swapなし、ルートdisk約61.3GB（提供時空き約35.8GB）。Linux x86_64 / Ubuntu 24.04.5、クロスコンパイルはlinux/amd64。

| SSH名 | 公開IPv4 | 実ホスト上のIPv4 | 固定region | CPU |
|---|---|---|---|---|
| asia1 | 52.194.142.112 | 192.168.1.10 | asia | c5.large相当 / Xeon Platinum 8275CL系 |
| europe1 | 35.79.66.224 | 192.168.2.10 | europe | c7i.large / Xeon Platinum 8488C |
| america1 | 18.179.184.114 | 192.168.3.10 | america | c7i.large / Xeon Platinum 8488C |
| asia2 | 52.199.210.183 | 192.168.4.10 | asia | c5.large / Xeon Platinum 8275CL |

SSHはisuconユーザー、既存のローカルSSH設定を使用。ポータルの登録情報には*.100のprivate addressが表示されたが、実インターフェースと提供ネットワーク定義は上表の*.10。アプリ接続先の値を混同しない。

公開入口は`http://13.192.72.111:8080/`およびルーターのHTTPS 443。ルーターは参加者の変更・SSH対象外。アプリ側は各region入口のHTTP 80を引き継ぐ。観測したnginxにTLS listenerや証明書参照はなく、参加者が配るTLS鍵は今回不要。TLS終端は管理外ルーターの責務。HTTPSの証明書名による検証は未試験。

クライアント通信・ホスト間通信はルーターを経由する公式制約。別経路でregionの遅延・帯域制限を迂回しない。asia2はネットワーク上の別サブネットだが、競技上asiaと同region。region間の遅延・帯域制限の具体値は公開資料から確定できず、推定値を設計入力にしない。

ルーターのX-Country-CodeはJP/KR/CN/AU→asia1、GB/DE/FR→europe1、US/CA/MX→america1。省略時JP。空、小文字、未対応、複数値・同名ヘッダー複数は400（公式）。所属regionを変えてもこの転送規則は変わらない。

静的ファイルは`public/`。`/`とSPAの非API pathはindex.html、`/assets/index-DVx2k2rA.js`、`/assets/index-DRoYlDJr.css`、`/isureal-logo.svg`を元のバイト列で配る（公式）。存在しない非API pathはindex.htmlへfallback。アプリ入口のrequest body上限は32MiB。静的応答は観測時ETag/Last-Modifiedあり、Cache-Control明示なし。画像APIはno-storeであり、静的資材の更新/初期化契約と混ぜない。圧縮・cache最適化は元の内容と公開仕様を守ってbootstrapで決める。

管理外の登録・環境検査、isuadmin、SSH鍵、固定regionを変更しない。`isureal-guard`はBANという機能の引継ぎが必要だが、旧unitを新構成で動かす要求ではない。Go単体の完成時に停止すべき旧サービスはTaskfileのDISABLED_SERVICESへ記載した。**setupでは停止・deploy・初期化をしていない**。APP_HOSTS/INGRESS_HOSTSは公開転送先3台を表す初期の設定であり、4台目の利用方法まで設計していない。RESET_INPUTS、APP_ENV、初期データの配布先・初期化への接続は、bootstrapが新設計の決定後に設定する。

## ベンチ・得点・取得可能な情報

ポータル`https://portal.isucon.net`。HARで観測した投入はPOST `/api/contestant/benchmark_jobs`、protobuf field 1=603（対象ID）。GET `/api/contestant/benchmark_jobs/<id>`で取得。対象IDは既存ポータルUIの指定であり、独自に単一ホストのみのベンチへ変えない。

`task portal-bench-check`で設定検査済み。Cookie/CSRF/base URL/対象IDはGit対象外`.env.local`、通信・結果抽出の宣言は`tools/contest/portal-bench.json`。HARの要求本文と一致すること、開始応答のID抽出、最終応答のstatus=4・passed=true・score=2993・stdout抽出をローカルHTTP再生で検査した。既存ジョブ116のread-only取得でも同じ結果を確認した。原文は`generated/portal-check/`（Git対象外）。これは既存初期実装の結果であり、新構成のPASSではない。認証期限切れ時は新HAR等からローカル資格情報を更新する。

protobufの状態はHARで0/1/4を観測。既存宣言の異常終端2/3はこのHARでは未観測。最終結果はfield 1.18.2（passed）、1.9（score）、1.18.5.2/3（stdout/stderr）を使用。ログは途中の応答では空で、完了応答でまとめて得られた。`tools/contest/analysis-schema/`で結果JSON、日次人数、時刻不明の完了時エラーメッセージを解釈する。実際の制御行付き保存形式でscore=2993、passed=true、日次人数16行、グランプリ人数3行（asia=0/europe=4/america=1）、summary message 1件を抽出検証した。進捗の相対時刻は分析DBのload_windowsから計算し、利用可能な負荷区間がなければnullにする。グランプリの地域別人数も`grand_prix_users.asia/europe/america`として解析し、0人を欠損扱いにしない。時刻付き日次人数の行は参加者向けstdoutにあるが、リアルタイムの負荷増減理由・未公開の内部メトリクスは取得していない。制御行BENCHMARK_START/END、SCORE、BENCHMARK_PASS/FAILと本文を区別する。

API分類は`tools/contest/user-transition-routes.json`に保存した。Bearer認証のためCookie由来のユーザー相関は現時点では取得できず、operation_pairs/operation_intervalsもbootstrapで計測へ接続する。ダミーのcookie_fieldはユーザー識別が可能という意味ではない。

新規ベンチは必ず`task bench`。配布物verify・role確認・共通操作ロック・RUN保存/回収を迂回しない。今回は旧実装に計測/配布検査を追加しないため新規RUNは実施していない。HARの実行済み結果は公式の初期確認の証拠にはなるが、計測付きRUNとして捏造しない。

公式の得点はΣ（検証成功の活動回数×配点×region倍率）、asia=1、europe/america=1.5、最後に小数切捨て。HTTP成功数そのものではない。FAILは0。直接エラー減点なし。最後に行ったベンチが最終スコア。

| 活動 | 配点 |
|---|---:|
| 投稿後、期間終了前の友達フィード検証成功（1人1期間1回） | 通常10、最後の期間20 |
| フィード/投稿詳細/交流一覧/プロフィール/通知/前回投稿Memoriesの確認 | 1 |
| 画像取得 | 2 |
| RealMoji送信 | 5 |
| コメント投稿 | 3 |
| 交流削除 | 1 |
| ユーザー検索、友達申請送信、承認 | 2 |
| 申請拒否/取消、友達解除、関係一覧、フォロー/解除、ブロック/解除 | 1 |
| 表示名/bio変更、caption変更 | 1 |
| プロフィール画像変更/削除 | 3 |
| 退会 | 5 |
| 月Memories / 日Memories / エクスポート | 3 / 2 / 20 |
| 過去画像取得 | 2 |

期間の閲覧活動は1人あたりフィード20、画像4、詳細8、交流一覧合計8、プロフィール4まで、RealMoji送信・コメント投稿は各5まで。投稿直後の画像/詳細確認はこの閲覧上限と別。Memories/エクスポートは返した件数でなく一連の検証成功回数。

依存関係は「期間開始→通知→投稿→友達フィード検証→閲覧・交流」、関係操作やプロフィール・履歴検証も別の活動。期間終了前の完了が投稿得点に必要。HTTPリダイレクトをベンチは追わない。503は再試行されるがエラー率・応答時間から除外されない。初期ログイン401、権限違反、初期履歴/画像不一致、削除5秒超の画像取得はクリティカルFAIL。終了時に強制切断された未完了リクエストは得点/検証に影響しない。

## 提供データと確認範囲

`data/initial/`に旧コード・内部定義と独立したgzip JSON Linesを置く。各行はUTF-8 JSON object。時刻はoffset付き、nullを維持。ファイルの全件数・SHA-256と検証結果は`data/initial/manifest.json`。業務属性の意味は上記の同名属性に対応する。秘密鍵は元の平文を必要とせず、secretKeySha256でログイン比較・画像署名鍵を再現できる。管理者もpasswordSha256を提供する。動作確認用の一般/管理者ログイン情報はGit対象外の`.env.local`にISUREAL_SMOKE_USERNAME、ISUREAL_SMOKE_SECRET、ISUREAL_ADMIN_USERNAME、ISUREAL_ADMIN_PASSWORDとして用意した。一般ユーザーのregionはasia。

| ファイル | 内容 |
|---|---|
| accounts.jsonl.gz | 初期5000人。id,username,secretKeySha256,displayName,bio,profileImageId,region,accountType,status,deletedAt,createdAt,updatedAt |
| administrators.jsonl.gz | 初期管理者。id,username,passwordSha256,createdAt。管理者のログイン情報はGit対象外のISUREAL_ADMIN_USERNAME/ISUREAL_ADMIN_PASSWORD |
| friend-requests.jsonl.gz | 申請ID、requesterId,recipientId,status,createdAt,updatedAt |
| friendships.jsonl.gz | 双方向関係のid,user1Id,user2Id,requestId,createdAt,updatedAt |
| follows.jsonl.gz | id,followerId,followeeId,createdAt,updatedAt |
| blocks.jsonl.gz | id,blockerId,blockedId,createdAt |
| posting-windows.jsonl.gz | WindowとendedAt,createdAt。1095件（各regionの2025-10-28〜2026-10-27）、初期currentは全regionでなし |
| posts.jsonl.gz | Postの保存値。authorId,postingWindowId,imageId、location、postedAt,createdAt,deletedAtを含む。authorや署名URLは現在の状態から返す |
| realmojis.part-NNN.jsonl.gz | id,postId,reactorId,imageId,reactionType,createdAt,replacedAt,deletedAt |
| comments.jsonl.gz | id,postId,authorId,body,createdAt,deletedAt |
| notifications.part-NNN.jsonl.gz | id,userId,postingWindowId,createdAt |
| images/index.part-NNN.jsonl.gz | 画像id,ownerId,mediaType,createdAt,part,offset,size,sha256 |
| images/manifest.json | 全画像数・合計byte・各partのサイズとSHA-256 |
| images/NNNN.bin | 元画像バイトの連結。indexのpartファイル中offsetからsize byteを読む。**ユーザー指定により画像本体だけGit対象外**。この作業環境に実体を保持 |

画像本体は新しいcheckoutへのGit cloneだけでは揃わない。別checkoutへ移る場合は`data/initial/images/*.bin`をこの作業環境からコピーし、`task verify-initial-data`で検証する。旧DBや旧コードへの接続は復元に不要。

メタデータはaccounts 5000、administrators 1、friend-requests 28970、friendships 24985、follows 10020、blocks 250、posting-windows 1095、posts 625111、realmojis 1199348、comments 546011、notifications 1825000件。RealMojiはcurrent 1090560件、replaced 108788件、deleted 0件。コメントは削除済み16379件を含む。ユーザー×日付の初期投稿数は未削除最大1、削除済み最大1で、各日の正確な初期値は全投稿から求められる。

初期アカウントのregion内訳はasia 4948、europe 23、america 29。certified_publicはそれぞれ8,1,1。退会済み113人は全員asia。これらも履歴・関係・画像の存在/有効性に影響するので除去しない。

元の初期化境界は2026-10-28T01:36:31.00958Z。初期アカウントと関係は保存済み初期値から、投稿期間・履歴・画像・通知はこの境界までに作成された提供値から取り出した。初期投稿の削除済み6210件は保持する。初期履歴の削除/反応置換がその後のベンチで変更されていないことは、境界後のdeletedAt/replacedAtが0件で確認した。captionには更新時刻の独立記録がなく、過去の変更有無はこの手段だけでは証明できない。ただし取得時点の提供状態をそのまま収録する。

受付の初期値は「全regionでcurrentなし→hasPosted=false,canPost=false,deletionCount=0」。将来の期間開始やregion移動先日付との照合に必要な**全localDateの削除済み/未削除投稿**を収録し、0埋めで代用しない。通知は履歴を残すが、初期接続時刻より前なのでSSEの即時再送対象ではない。初期セッション/管理者セッション/BANは空として復元する。

画像は1826950件、原本合計13713322751 byte（約12.8GiB）。

`task verify-initial-data`は全件数・参照関係・重複・状態・日付ごとの投稿/削除数・画像位置/内容hashを確認する。結果はmanifestのvalidation。初期化がこれらの元データを読み込めることと、APIの全業務判定・競合が正しく実装されたことは別であり、後者を検証済みとはしない。静的資材も`tools/contest/static-manifest.json`のSHA-256と照合する。画像の全件decode試験は行わず、転送した内容のhash・offset・byte数の整合性を確認する。提供元の画像件数・総byte数とも照合する。提供元とのSHA-256/長さの比較は先頭20画像で一致し、公開APIとの実バイト比較はエクスポート先頭の1画像に限る。

公開HTTPではSPA（200）、提供ユーザーの認証（200）、自己情報（200）、current（200）、2026-10のMemories（200、3件）、エクスポート（200、29投稿・30 ZIP entry）を確認した。エクスポート先頭の画像1件と署名URLのバイト列が一致し、同じトークンで別regionの自己情報を読むと403になることも観測した。観測時は以前のベンチによる現行期間が残っており、初期化後の状態を観測したものではない。関係変更・退会・投稿・期間開始の全シナリオや競合はソース照合の範囲であり、bootstrapの正当性確認に残る。

## 次セッションの入口

初期ソースを読んでいない新しいセッションで`$isucon-bootstrap docs/behavior.md`を実行する。このsetupの会話・調査ディレクトリをそのセッションに渡さない。Taskfile、提供データ、本書、静的資材、ポータル設定を入力とする。bootstrapは新実装の正規deploy、`task initialize`、全機能/再起動の正当性確認、最初の`task bench` PASSと計測・回収の確認まで行う。
