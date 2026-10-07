# OVC – Oracle Version Control

> Tài liệu đặc tả (specs) – phiên bản 0.1 (draft)
> Ngôn ngữ: Go · Database: Oracle 19c · Git host: **GitHub** ở giai đoạn đầu, sau đó chuyển sang **GitLab của công ty** bằng cách đổi remote.
>
> Quy ước: trong tài liệu này "GitLab" nói chung là **Git host** (GitHub hoặc GitLab). Phần nào khác nhau giữa hai bên được ghi rõ (§4.3, §10).

---

## 1. Bối cảnh & mục tiêu

### 1.1 Bối cảnh

- Dự án làm theo hướng **DB-first**: API chỉ gọi procedure/package trên Oracle, phần lớn business logic nằm trong PL/SQL.
- Code trên DB rất lớn nhưng **không có version control**: sửa trực tiếp trên DB, đè code của nhau, không biết ai sửa gì, khi nào, không rollback được.

### 1.2 Mục tiêu

1. Toàn bộ source của các schema Oracle nằm trong **một repository có sẵn** trên Git host; **mỗi branch dài hạn là một DB** (`dev`, `prd`), trong branch **mỗi schema (owner) là một thư mục** (`HR/`, `QLSC/`).
2. Dev làm việc giống code thông thường: **pull → sửa local → push → Merge Request → review → merge**.
3. **Conflict được phát hiện và giải quyết trên file**, không còn chuyện đè code trên DB.
4. **Mọi thay đổi DB chỉ đi qua một luồng duy nhất**: merge vào branch → GitLab pipeline deploy.
5. Biết được **ai, sửa gì, khi nào, deploy lên đâu** (audit).
6. Phát hiện được thay đổi ngoài luồng (**drift**) giữa DB và Git, và **đồng bộ ngược** thay đổi đó từ DB về Git khi cần (sửa nóng lúc sự cố).

### 1.3 Ngoài phạm vi (giai đoạn đầu)

- Version control cho **dữ liệu** (chỉ quản lý cấu trúc + code, trừ dữ liệu master nhỏ nếu cần ở phase sau).
- Tự động sinh script `ALTER TABLE` từ việc so sánh 2 phiên bản bảng.
- Giao diện web riêng (dùng giao diện GitLab cho MR/review/pipeline).
- Tự động rollback thay đổi cấu trúc bảng.

---

## 2. Thuật ngữ

| Thuật ngữ | Ý nghĩa |
|---|---|
| **OVC CLI** (`ovc`) | Công cụ dòng lệnh chạy trên máy dev, 1 file binary. |
| **OVC Server** (`ovc-server`) | Server trung gian: nhận yêu cầu từ CLI, thao tác Git/GitLab, đọc DB. |
| **DB branch** | Branch dài hạn ứng với **một** DB, tên là alias của DB (`dev`, `prd`). Chứa source của mọi schema đã init trên DB đó, mỗi schema một thư mục `<OWNER>/` (§5). |
| **Owner baseline** | Commit gốc riêng của một schema, chỉ chứa thư mục `<OWNER>/` (bộ file vỏ) tạo lúc init đầu tiên của schema; được merge vào mọi DB branch để các branch có chung tổ tiên với từng file của schema (§9.1). |
| **Workspace** | Thư mục local trên máy dev chứa bản sao của một schema branch. |
| **Code object** | Object tạo lại được bằng `CREATE OR REPLACE`: package, procedure, function, view, trigger, type. |
| **Migration** | Script chạy **một lần** để đổi cấu trúc/dữ liệu (`ALTER TABLE`, data fix...). |
| **Drift** | Khác biệt giữa DB thực tế và source trên Git. |
| **DB sync** | Đưa thay đổi ngoài luồng trên DB về Git (giống "sync fork"), merge 3 chiều với branch của env (§9.7). |
| **Stub (vỏ)** | File chỉ có tên object và một dòng đánh dấu `-- OVC:STUB`, chưa có nội dung thật (§5.5). |
| **Hydrate** | Kéo nội dung thật của object từ DB về Git, thay cho stub (§9.8, lệnh `ovc get`). |
| **Last deployed SHA** | Commit đã deploy gần nhất lên một env, đọc từ `OVC_ADMIN.OVC_DEPLOY_LOG`. |
| **Environment (env)** | Một DB đích. Tên env = alias DB trong cấu hình server = tên DB branch: `dev`, `prd`. |
| **Git host** | Nơi đặt repo, PR/MR và CI: GitHub (giai đoạn đầu) rồi GitLab công ty. |

---

## 3. Nguyên tắc thiết kế

1. **Git là nguồn sự thật (source of truth).** DB chỉ là nơi chạy code được deploy từ Git.
2. **Một luồng thay đổi duy nhất – bằng quy ước.** DB chỉ thay đổi qua GitLab pipeline; cả nhóm thống nhất không sửa tay, kể cả hotfix. Hệ thống **không chặn kỹ thuật** (không dùng trigger DDL): việc giữ kỷ luật dựa vào thoả thuận, còn **drift check** phát hiện vi phạm. Nếu vẫn có sửa tay (sự cố khẩn cấp), dùng `ovc sync` để đưa về Git ngay – Git không bao giờ được lệch lâu với DB.
3. **Phân tách quyền:**
   - OVC Server **chỉ có quyền ĐỌC** DB (export, diff, drift).
   - **Chỉ GitLab pipeline giữ key GHI DB** (deploy): account deploy của từng dự án nằm trong CI variables của dự án đó.
   - Dev **không được cấp** account có quyền DDL trên DB dùng chung và không cần biết mật khẩu DB.
4. **Dev làm việc bằng git như source code thường.** Lấy source bằng `git clone`, sửa, tạo branch, `git push`, mở PR/MR. OVC CLI chỉ dùng cho việc git không làm được: kéo nội dung thật của object (`ovc get`), init, drift, DB sync, và deploy trong pipeline. CLI không kết nối trực tiếp Oracle.
5. **Quyền do Git host quyết định, OVC chưa có bộ user riêng.** Dev dùng **tài khoản Git host của mình** để clone và push feature branch. DB branch (`dev`, `prd`) được bảo vệ: chỉ thay đổi qua PR/MR, ai được review/approve/merge do Git host (branch protection, approval rule) kiểm soát. OVC Server dùng một tài khoản dịch vụ (service account) chỉ để ghi các commit do OVC sinh ra lên DB branch: init, hydrate, DB sync. Danh tính trên các commit này do CLI khai báo (§8), không được xác thực.
6. **Không tự giải conflict thay người dùng.** OVC chỉ merge khi sạch; có conflict thì trả về cho dev xử lý.

---

## 4. Kiến trúc tổng thể

```
 Developer
    │  git clone/pull/push/MR (thẳng với Git host)   ovc get / init / drift / sync (qua OVC Server)
    ▼
┌──────────┐        HTTPS         ┌────────────────────┐   git + GitLab API   ┌──────────────────┐
│ OVC CLI  │ ───────────────────► │ OVC Server         │ ───────────────────► │ GitLab công ty   │
│ (local)  │ ◄─────────────────── │ (trung gian)       │ ◄─────────────────── │ repo / MR / CI   │
└──────────┘   source, kết quả    │ - git mirror repo  │                      │                  │
                                  │ - metadata DB      │                      │  Runner          │
                                  │ - audit log        │                      │  (giữ key ghi DB)│
                                  │ - DB READ-ONLY     │                      └──────────────────┘
                                  └────────────────────┘                               │
                                          │ SQL*Net (chỉ đọc:                          │ SQL*Net (ghi:
                                          │ init, drift)                               │ deploy)
                                          ▼                                            ▼
                                  ┌──────────────────────────────────────────────────────────┐
                                  │              Oracle 19c (dev / prd)               │
                                  └──────────────────────────────────────────────────────────┘
```

### 4.1 Thành phần

| Thành phần | Vai trò | Ghi chú |
|---|---|---|
| `ovc` (CLI) | Việc git không làm được: `ovc get` (kéo nội dung thật của object), init, drift, DB sync; chế độ deploy trong pipeline. Lấy source, sửa, push, PR/MR: dev dùng git và Git host như source code thường. | 1 binary cho Windows/Linux/macOS. Cần `git` trên máy (để `ovc get` kéo commit về); không cần Go hay Oracle Client. |
| `ovc-server` | API cho CLI; quản lý bản mirror của repo; ghi commit init/hydrate/sync lên DB branch; export DDL; kiểm tra drift; audit. | Chạy trong Docker trên VM nội bộ. Cần `git` cài trên server. |
| Metadata DB của OVC | Lưu danh sách schema đã đăng ký, cấu hình DB, audit log, lock. | PostgreSQL (SQLite cho môi trường dev/test của tool). |
| Git host (GitHub → GitLab công ty) | Repo, PR/MR, review, phân quyền merge, CI/CD. OVC Server dùng một service account có quyền push. | Không sửa Git host, chỉ dùng `git` + API tạo PR/MR + CI. |
| Runner CI (GitHub Actions runner / GitLab Runner) | Chạy pipeline deploy trong mạng nội bộ. Giữ credential ghi DB trong secret của environment. | Chạy `ovc deploy` (cùng binary CLI, chế độ deploy). |
| Oracle 19c | DB đích. | Có schema quản trị `OVC_ADMIN` để lưu lịch sử deploy. |

### 4.2 Luồng tổng quát

```
git clone (branch dev) ─────────────────────────────► Git host
ovc get <object> ──► OVC Server ──► đọc DDL từ DB ──► commit lên dev (Git host)
                 └─► git fetch + git merge origin/dev (kéo commit hydrate về local)
dev sửa file, git checkout -b feature/..., git commit, git push ──► Git host
mở PR/MR vào dev; review + merge trên Git host
merge vào dev / prd ──────────► Pipeline ──► ovc deploy ──► Oracle (dev/prd)
định kỳ: ovc-server drift ──► so DB với Git ──► báo cáo
khi DB bị sửa tay: ovc sync ──► OVC Server đọc DB ──► merge 3 chiều ──► Git host (branch của DB)
```

---

### 4.3 Git host: GitHub trước, GitLab sau

OVC chỉ cần từ Git host những thứ sau, nên chuyển host chỉ là đổi cấu hình:

| Việc | Cách làm | Phụ thuộc host? |
|---|---|---|
| Mirror, commit init/hydrate/sync, push (server) | Lệnh `git` qua HTTPS bằng token của service account | **Không** – chỉ đổi `git.remote` |
| Tạo / liệt kê PR hoặc MR | API của host (GitHub: pull request; GitLab: merge request) | **Có** – một interface nhỏ, mỗi host một bản cài đặt |
| Bảo vệ branch (`dev`, `prd`), review/approve | Admin cài sẵn trên host | Có, nhưng ngoài OVC |
| Pipeline deploy | File CI trong branch (GitHub Actions hoặc GitLab CI), cùng gọi `ovc deploy` | Có – §10.2 |
| Secret deploy theo environment | Environment secrets (GitHub) / biến theo environment scope (GitLab) | Có – §10.3 |

Interface host (Go) chỉ có: `CreateChangeRequest(source, target, title, body)` và `ListChangeRequests(...)`. Chọn host bằng `git.host: github | gitlab` trong cấu hình (§13.1).

---

## 5. Cấu trúc repo và branch

Repo trên Git host **có sẵn và dùng chung** cho mọi schema (OVC không tạo repo). **Mỗi DB là một branch dài hạn**, tên branch là alias của DB trong cấu hình server (§13.1): `dev`, `prd`. Trong branch, **mỗi schema (owner) là một thư mục** đặt tên đúng như Oracle lưu (`HR/`, `QLSC/`; tên có chữ thường mã hoá như §5.1).

- Thư mục `<OWNER>/` của một schema chỉ xuất hiện trên DB branch sau khi chạy `ovc init <OWNER> --db <alias>` cho DB đó (§9.1); mỗi DB init riêng, từ chính DB đó.
- Lần init đầu tiên của schema tạo **owner baseline**: một commit gốc chỉ chứa `<OWNER>/` (file vỏ + `ovc.yaml`). Commit này được **merge vào mọi DB branch** khi init schema trên DB đó, nên mọi file của schema trên `dev` và `prd` có chung tổ tiên là bản vỏ, và PR/MR `dev` → `prd` merge sạch. Không lấy thẳng commit trên `dev` vì lịch sử của nó kéo theo thư mục của các schema khác.
- File CI nằm ở gốc branch, tạo một lần khi branch được tạo.
- Feature branch đặt tên `feature/<owner>/<user>/<tên>` (owner viết thường), tách từ `dev` (hoặc `prd` cho hotfix). Mỗi feature branch chỉ sửa trong thư mục của một schema (CLI làm việc theo schema, §6).

Nội dung một branch (ví dụ `dev`):

```
(gốc branch dev)
├── .github/workflows/ovc-deploy.yml   # gọi workflow chung (GitHub Actions)
├── .gitlab-ci.yml              # include template chung (dùng khi chuyển sang GitLab)
├── QLSC/                       # schema khác, cùng cấu trúc
└── HR/
    ├── ovc.yaml                    # cấu hình của schema (quy tắc deploy, exclude)
    ├── tables/                     # snapshot cấu trúc bảng hiện tại (tham chiếu)
    │   └── EMPLOYEES.sql
    ├── indexes/
    │   └── EMPLOYEES_IX1.sql
    ├── constraints/                # FK tách riêng để tránh phụ thuộc thứ tự
    │   └── EMPLOYEES.sql
    ├── sequences/
    │   └── EMPLOYEES_SEQ.sql
    ├── views/
    │   └── V_EMPLOYEE_INFO.sql
    ├── materialized_views/
    ├── types/
    │   ├── T_EMP_REC.tps           # type spec
    │   └── T_EMP_REC.tpb           # type body
    ├── packages/
    │   ├── PKG_EMPLOYEE.pks        # package spec
    │   └── PKG_EMPLOYEE.pkb        # package body
    ├── procedures/
    │   └── P_SYNC_EMPLOYEE.prc
    ├── functions/
    │   └── F_GET_SALARY.fnc
    ├── triggers/
    │   └── TRG_EMPLOYEES_BIU.trg
    ├── synonyms/
    │   └── SYN_DEPT.sql
    ├── grants/
    │   └── grants.sql              # grant mà schema này cấp cho schema khác
    ├── jobs/                       # DBMS_SCHEDULER jobs (phase sau)
    └── migrations/
        ├── V20261006_1530__add_column_email.sql
        └── V20261007_0910__fix_status_data.sql
```

### 5.1 Quy tắc đặt tên file

- Tên file = **tên object đúng như Oracle lưu**. Tên không có chữ thường (trường hợp thường gặp) dùng nguyên. Tên quoted có chữ thường được **mã hoá** để không bị trùng trên filesystem không phân biệt hoa thường (macOS, Windows), vì Oracle coi `KiemTra_MST` và `KIEMTRA_MST` là hai object khác nhau: mỗi đoạn chữ thường được bao bởi `~`, ký tự không phải chữ cái không đổi chế độ. Ví dụ `KiemTra_MST` → `K~iem~T~ra_~MST.fnc`, `lay_dbtb` → `~lay_dbtb~.prc`. Giải mã bằng cách bỏ mọi `~`; tên file phải đúng dạng chuẩn. Tên chứa `~`, `/`, `\` hoặc ký tự điều khiển không biểu diễn được: `ovc init` bỏ qua và cảnh báo.
- Mỗi file chứa **đúng 1 object** (riêng package/type tách spec và body thành 2 file).
- Phần mở rộng theo loại object:

| Loại object | Thư mục | Đuôi file |
|---|---|---|
| TABLE | `tables/` | `.sql` |
| INDEX | `indexes/` | `.sql` |
| CONSTRAINT (FK) | `constraints/` | `.sql` (gom theo bảng) |
| SEQUENCE | `sequences/` | `.sql` |
| VIEW | `views/` | `.sql` |
| MATERIALIZED VIEW | `materialized_views/` | `.sql` |
| TYPE / TYPE BODY | `types/` | `.tps` / `.tpb` |
| PACKAGE / PACKAGE BODY | `packages/` | `.pks` / `.pkb` |
| PROCEDURE | `procedures/` | `.prc` |
| FUNCTION | `functions/` | `.fnc` |
| TRIGGER | `triggers/` | `.trg` |
| SYNONYM | `synonyms/` | `.sql` |

### 5.2 Định dạng nội dung file

- Encoding **UTF-8**, xuống dòng **LF**, không có khoảng trắng thừa cuối dòng.
- **Không ghi tên schema** trong DDL (`CREATE OR REPLACE PACKAGE PKG_EMPLOYEE`, không phải `HR.PKG_EMPLOYEE`) để deploy được sang schema/DB khác.
- Code object bắt đầu bằng `CREATE OR REPLACE` và kết thúc bằng `/`.
- Không chứa storage/tablespace/segment attributes (để không lệch giữa các môi trường).

### 5.3 Hai loại thay đổi

| Loại | Áp dụng cho | Cách deploy | Khi file thay đổi |
|---|---|---|---|
| **Repeatable** | package, procedure, function, view, trigger, type, synonym | Chạy lại toàn bộ file (`CREATE OR REPLACE`) | Deploy lại file đó |
| **Versioned (migration)** | table, index, constraint, sequence, dữ liệu | Chạy **1 lần duy nhất**, ghi lại vào `OVC_ADMIN.OVC_MIGRATION_LOG` | Không được sửa migration đã deploy |

- Thư mục `tables/`, `indexes/`, `constraints/`, `sequences/` là **snapshot tham chiếu** cấu trúc hiện tại: dev cập nhật cùng MR với migration, pipeline **không chạy** các file này (trừ khi dựng môi trường mới từ đầu – `ovc deploy --bootstrap`). Drift check dùng snapshot này để so với DB.
- Tên migration: `V<yyyyMMdd>_<HHmm>__<mô_tả>.sql`, sinh bằng `ovc new migration <mô_tả>`. Dùng timestamp thay vì số thứ tự để 2 dev không đặt trùng tên.

### 5.4 File `ovc.yaml`

```yaml
schema: HR
version: 1
deploy:
  allow_drop: false          # xoá file code object có sinh DROP không
  recompile: true            # compile lại object invalid sau deploy
  fail_on_new_invalid: true  # pipeline fail nếu deploy làm phát sinh object invalid mới
exclude:                     # object không quản lý
  - "SYS_*"
  - "BIN$*"
  - "MLOG$_*"
  - "RUPD$_*"
  - "TMP_*"
sync:
  auto_sync: false           # drift check định kỳ tự chạy DB sync nếu phát hiện drift (§9.7)
  envs: [dev, prd]           # env được phép sync ngược về Git
```

### 5.5 File vỏ (stub) và hydrate

`ovc init` **không export nội dung**. Mỗi object trên DB tạo ra một file đúng tên theo §5.1, nhưng nội dung chỉ là một dòng đánh dấu:

```
-- OVC:STUB type=PACKAGE_BODY
```

- Dòng stub **giống hệt nhau ở mọi DB** (chỉ có loại object), để commit init trên DB khác chỉ thêm/xoá file chứ không sửa file stub, và merge giữa các branch không bị xung đột.
- `last_ddl_time` lúc init được lưu **ở server, riêng cho từng schema + DB** (§12), không nằm trong file. Dùng để cảnh báo khi object đã đổi trên DB kể từ init (§9.6, §9.8).
- Stub **không bao giờ được deploy** (§10.4) và **không được sửa trực tiếp**: muốn sửa object nào thì phải `ovc get` object đó trước (§9.8).
- Khi hydrate, server thay stub bằng DDL thật lấy từ DB đang làm (chuẩn hoá như §9.8) và commit lên **branch của DB đó**. Từ đó file là file thường; DB khác nhận nội dung qua PR/MR như mọi thay đổi khác.
- Cấu trúc thư mục (tên và loại object) lúc nào cũng đầy đủ, nên dev nhìn repo là biết schema có gì, và `git log`, `git diff` hoạt động bình thường.

---

## 6. Working copy của dev

Working copy là một **`git clone` bình thường** của repo, checkout branch DB (thường `dev`). Không có workspace riêng của OVC.

```
~/work/oracle-src/          # git clone ... --branch dev
├── .github/workflows/...
├── HR/
│   ├── ovc.yaml
│   ├── packages/PKG_EMPLOYEE.pkb   # còn là vỏ cho tới khi ovc get
│   └── ...
└── QLSC/
```

- Dev cần tài khoản Git host có quyền clone repo và push feature branch (không push được lên `dev`, `prd`).
- Muốn sửa một object còn là vỏ: `ovc get` object đó trước (§9.8). Sửa trực tiếp file vỏ bị pipeline `validate` từ chối (§9.4).
- CLI **không có file cấu hình**: địa chỉ OVC Server gắn sẵn trong binary (§13.2), danh tính lấy từ `git config user.name` / `user.email` (cùng danh tính ký commit của dev).

---

## 7. OVC CLI – đặc tả lệnh

Cú pháp chung: `ovc <lệnh> [tham số] [--flag]`. Mọi lệnh hỗ trợ `--json` (output máy đọc) và `--verbose`.

### 7.1 Danh tính & phiên bản

| Lệnh | Mô tả |
|---|---|
| `ovc whoami` | Hiện danh tính (từ `git config user.name` / `user.email`), server, phiên bản. |
| `ovc version` | Phiên bản CLI và server; cảnh báo nếu không tương thích. |
| `ovc update` | Tự tải bản CLI mới nhất từ server và thay thế binary. |

### 7.2 Schema

| Lệnh | Mô tả |
|---|---|
| `ovc schemas` | Liệt kê schema đã đăng ký và các DB đã init. |
| `ovc init <SCHEMA> --db <alias>` | Liệt kê object của schema từ DB (chỉ tên, loại, `last_ddl_time`) và thêm thư mục `<SCHEMA>/` gồm **bộ file vỏ** (§5.5) + `ovc.yaml` vào branch của DB đó (tạo branch nếu chưa có). Lần đầu của schema: tạo owner baseline. DB sau: merge owner baseline, thêm một commit cân theo DB đó. Không export nội dung nên chạy rất nhanh. Thêm `--full` nếu muốn export đầy đủ ngay (hydrate toàn bộ). |

### 7.3 Kéo nội dung object (chạy trong git working copy)

| Lệnh | Mô tả |
|---|---|
| `ovc get <file\|thư mục\|[OWNER.]TÊN>...` | Đảm bảo object có **nội dung thật trên remote**, rồi kéo về local (§9.8). File còn là vỏ trên remote → server hydrate từ DB và commit lên branch DB; đã có nội dung → bỏ qua bước này. Sau đó `git fetch` và đưa branch DB vào branch hiện tại (fast-forward nếu được, không thì `git merge`). Ví dụ: `ovc get HR/packages/PKG_EMPLOYEE.pkb`, `ovc get HR.PKG_EMPLOYEE` (cả spec và body), `ovc get HR/procedures/` (mọi procedure), `ovc get HR` (cả owner). Với bảng, kéo kèm index và constraint. |
| `ovc get ... --db <alias>` | Branch DB để hydrate và kéo về (mặc định `dev`; `prd` cho hotfix). |
| `ovc get ... --type <loại>` | Chỉ lấy một loại object trong thư mục/owner (vd `--type procedure`). |
| `ovc get ... --no-merge` | Chỉ hydrate và `git fetch`, không đụng branch hiện tại (dev tự `git merge`/`rebase`). |
| `ovc new migration <mô_tả>` | Sinh file migration theo chuẩn tên. |

Lấy source, sửa, commit, tạo branch, push, PR/MR: dùng `git` và giao diện Git host (§9.2–9.5).

### 7.4 Theo dõi DB

| Lệnh | Mô tả |
|---|---|
| `ovc drift --env <env>` | Yêu cầu server so DB môi trường `env` với commit đã deploy gần nhất, hiện kết quả. |
| `ovc sync --env <env> [--object O...] [--dry-run]` | Đồng bộ thay đổi ngoài luồng trên DB về branch của env, như "sync fork" (xem §9.7). `--dry-run` chỉ in danh sách object sẽ được sync. |

### 7.5 Chế độ deploy (chỉ dùng trong pipeline)

| Lệnh | Mô tả |
|---|---|
| `ovc deploy [--db <alias>]` | Deploy commit hiện tại lên DB. DB **suy ra từ tên branch**; schema cần deploy suy ra từ các thư mục `<OWNER>/` có thay đổi (`--db` chỉ để ghi đè/dry-run). Đọc thông tin kết nối từ biến môi trường của CI. Xem §10. |
| `ovc deploy --dry-run` | Chỉ in kế hoạch deploy (file nào, thứ tự nào), không chạy. |
| `ovc deploy --bootstrap` | Dựng schema trống từ snapshot + toàn bộ migration (môi trường mới). |
| `ovc deploy --record-sync` | Chỉ ghi nhận commit DB sync là đã deploy (`STATUS = SYNCED`), không chạy DDL (xem §9.7). |
| `ovc deploy --mark-applied <migration...>` | Ghi migration vào `OVC_MIGRATION_LOG` mà không chạy – dùng cho env mà thay đổi đã được sửa tay trước đó. |

### 7.6 Mã thoát (exit code)

| Code | Ý nghĩa |
|---|---|
| 0 | Thành công |
| 1 | Lỗi chung |
| 2 | Sai tham số |
| 3 | Git chưa có danh tính (`git config user.name`/`user.email`), binary không có địa chỉ server, hoặc không kết nối được server |
| 4 | Git host từ chối (service account thiếu quyền, hoặc `git fetch` của dev bị từ chối) |
| 5 | Conflict (vd `git merge` khi `ovc get` không merge được) |
| 6 | Lỗi deploy (lỗi SQL / object invalid) |
| 7 | Phát hiện drift |

---

## 8. OVC Server – đặc tả API

- REST/JSON qua HTTPS, prefix `/api/v1`.
- **Chưa có xác thực và phân quyền người dùng** trong OVC (bản đầu). Server nằm trong mạng nội bộ; rủi ro và hướng bổ sung xem §11.4 và §18 #6.
- **Danh tính khai báo:** CLI gửi header `X-OVC-User-Name` và `X-OVC-User-Email` (từ `git config user.name` / `user.email` trên máy dev). Server dùng làm tác giả commit và ghi vào audit; không xác minh.
- Server chỉ ghi lên Git host các commit do OVC sinh ra (init, hydrate, DB sync), bằng **service account** đã đăng nhập sẵn. Feature branch, push, PR/MR của dev đi thẳng giữa git của dev và Git host, không qua server.
- Header `X-OVC-Client-Version` bắt buộc; server trả `426 Upgrade Required` nếu CLI quá cũ.

| Method | Endpoint | Mô tả |
|---|---|---|
| GET | `/api/v1/version` | Phiên bản server, phiên bản CLI tối thiểu, link tải CLI. |
| GET | `/api/v1/schemas` | Danh sách schema user có quyền. |
| POST | `/api/v1/schemas` | Init schema trên một DB `{schema, db_alias}`: thêm `<SCHEMA>/` vào branch `<alias>`. Chạy bất đồng bộ, trả `job_id`. Schema đã init trên DB đó → `409 ALREADY_INITIALIZED`. |
| GET | `/api/v1/jobs/{id}` | Trạng thái job dài (init, drift, DB sync). |
| POST | `/api/v1/schemas/{s}/hydrate` | Hydrate `{db, paths[], all, types[]}` (đường dẫn tính từ thư mục owner): đọc nội dung thật từ DB cho các file **còn là vỏ trên branch**, commit lên branch của DB đó → `job_id`. Kết quả: commit SHA, file đã hydrate, file bỏ qua (đã có nội dung), file lỗi, cảnh báo (§9.8). |
| POST | `/api/v1/schemas/{s}/drift` | Chạy drift check `{env}` → `job_id`. |
| POST | `/api/v1/schemas/{s}/db-sync` | Chạy DB sync `{env, objects[], dry_run}` → `job_id`. Kết quả: `synced` (commit SHA), `conflict` (link MR) hoặc `no_change`. |
| GET | `/api/v1/audit?schema=&user=&from=&to=` | Tra cứu audit log (admin). |
| GET | `/healthz`, `/readyz` | Health check. |

Lỗi trả về dạng chuẩn:

```json
{ "error": { "code": "DB_NOT_INITIALIZED", "message": "schema HR is not on db prd (have [dev])" } }
```

---

## 9. Các luồng chi tiết

### 9.1 Init schema (`ovc init HR --db dev`)

Mỗi lần init thêm thư mục `<SCHEMA>/` vào **một** DB branch, từ chính DB đó (`--db dev` → branch `dev`). Init trên DB khác là một lệnh riêng (`ovc init HR --db prd`), chạy lúc nào cũng được, thứ tự nào cũng được.

1. Kiểm tra alias DB có cấu hình và schema chưa init trên DB đó (đã có → từ chối, `409`). Trên branch đã có thư mục `<SCHEMA>/` mà server không biết → từ chối, không ghi đè.
2. Server kết nối DB bằng user **read-only** `OVC_READER`.
3. Lấy danh sách object từ `DBA_OBJECTS` (lọc theo `exclude`, bỏ object do hệ thống sinh: `GENERATED='Y'`, recycle bin, LOB/IOT segment...), kèm `object_type`, `object_name`, `last_ddl_time`. Chỉ một câu truy vấn, **không gọi `DBMS_METADATA`**.
4. Với mỗi object, tạo file vỏ `<SCHEMA>/<thư mục>/<tên>.<đuôi>` theo §5.1, nội dung là dòng `-- OVC:STUB type=...` (§5.5). Index/constraint của bảng cũng tạo file vỏ (constraint gom theo bảng).
5. Ghi vào branch:
   - **Owner baseline:** lần init đầu của schema tạo commit gốc (không cha) chỉ chứa `<SCHEMA>/` (file vỏ + `ovc.yaml`). Các lần sau dùng lại commit này.
   - **Branch chưa có:** tạo branch tại owner baseline, thêm commit file CI ở gốc branch (nếu cấu hình CI).
   - **Branch đã có:** tạo merge commit (cha 1: đầu branch, cha 2: owner baseline) có cây = cây hiện tại + thư mục `<SCHEMA>/` của baseline. Không đụng file nào khác.
   - **DB không phải DB của lần init đầu:** thêm một commit cân theo DB này: thêm vỏ cho object chỉ DB này có, xoá file của object DB này không có. File có ở cả hai bên giữ nguyên (stub giống hệt nhau, §5.5). Không có khác biệt thì không thêm commit.
   Commit có trailer `OVC-User` và `OVC-Init: <SCHEMA> db=<alias>`. Push lên repo có sẵn bằng service account (đầu branch đổi trong lúc làm → làm lại từ đầu branch mới). Push lỗi → huỷ (branch mới tạo thì xoá, branch có sẵn thì giữ nguyên), không đăng ký gì. Với `--full`: bước này kèm hydrate toàn bộ object theo §9.8 bước 2 (worker pool mặc định 8 kết nối để xử lý schema hàng nghìn object).
6. Pipeline chạy `ovc deploy --baseline` cho schema trên DB đó để đánh dấu commit này là đã deploy (không chạy gì).
7. Bảo vệ branch `dev`, `prd` do admin cài **một lần**, không làm theo từng schema. Service account OVC phải được phép tạo và push các branch này (bypass; xem §11.3).
8. Ghi schema/DB vào metadata DB kèm `last_ddl_time` của từng object (§12), ghi audit.

> Init lần lượt từng DB (không init sẵn mọi DB từ một DB) để branch của mỗi DB phản ánh đúng DB đó, kể cả khi các DB đã lệch nhau trước khi dùng OVC. Mọi DB vẫn merge cùng owner baseline để có tổ tiên chung khi merge.


### 9.2 Lấy source

`git clone <repo> --branch dev` bằng tài khoản Git host của dev. Toàn bộ thư mục của mọi schema trên DB đó về máy, phần lớn là file vỏ nên rất nhẹ. Cập nhật về sau bằng `git pull`.

### 9.3 Làm thay đổi

1. `ovc get` các object cần sửa (§9.8): đảm bảo có nội dung thật trên remote và commit hydrate đã nằm trong branch local.
2. `git checkout -b feature/<owner>/<user>/<tên>` từ `dev` (hoặc từ `prd` cho hotfix).
3. Sửa file, thêm migration (`ovc new migration`), `git commit`.
4. Cần thay đổi mới của `dev`: `git pull origin dev` (hoặc `rebase`). Conflict do dev xử lý bằng git như mọi source khác.

> Vì sao `ovc get` phải đưa **commit hydrate** vào lịch sử branch, không chỉ chép nội dung file: nếu chỉ chép, commit của dev ghi "vỏ → nội dung + phần sửa" còn `dev` ghi "vỏ → nội dung", hai phía cùng sửa một đoạn nên PR/MR luôn conflict. Khi commit hydrate đã có trong lịch sử, diff của PR/MR chỉ còn phần sửa thật.

### 9.4 Push và kiểm tra

1. `git push -u origin feature/...` bằng tài khoản của dev (không push được lên `dev`, `prd`: branch bảo vệ).
2. Mở PR/MR vào branch DB (`dev`; hotfix vào `prd`) trên Git host. Pipeline `validate` chạy `ovc deploy --dry-run` và kiểm tra:
   - Đường dẫn file nằm trong thư mục cho phép, đúng đuôi theo §5.1; mỗi thư mục owner khớp `schema:` trong `ovc.yaml` của nó.
   - Tên object trong nội dung khớp tên file và loại object khớp thư mục.
   - Không chứa tên schema cứng (cảnh báo), không chứa marker conflict.
   - UTF-8 hợp lệ, kích thước file ≤ giới hạn (mặc định 5 MB).
   - Migration đã có trong branch đích thì **không được sửa/xoá**.
   - File đang là **vỏ** trên branch đích không được sửa (yêu cầu `ovc get` trước); nội dung mới không được mang dòng `-- OVC:STUB`.
   - File đang có **MR sync mở** (còn branch `sync/<db>/...` của object đó, §9.8) thì không được sửa: phải giải quyết MR sync trước.
   Lỗi kiểm tra → pipeline fail, PR/MR không merge được (bật "pipeline phải pass" trên Git host).
3. Tác giả commit là tài khoản git của dev; audit lấy từ Git host.

### 9.5 Merge Request & review

- Review, comment, approve, merge trên **giao diện Git host** bằng tài khoản của người review; ai có quyền merge do Git host quyết định (đây là chốt chặn duy nhất để code vào branch dài hạn).
- Merge vào `dev` → pipeline deploy lên DB dev; PR/MR `dev` → `prd` → deploy prd sau bước duyệt tay (§10).
- Conflict ở mức PR/MR (feature branch cũ hơn `dev`): dev `git pull origin dev`, xử lý conflict, `git push`.

### 9.6 Drift check

1. Server export DDL từ DB của `env` (như §9.1 bước 3–5) vào thư mục tạm.
2. So với **commit đã deploy gần nhất** lên env (đọc `OVC_DEPLOY_LOG`), không phải đầu branch – vì branch có thể có commit đã merge nhưng chưa deploy (vd `prd` deploy manual). Chỉ so trong thư mục `<SCHEMA>/` của branch DB.
3. Báo cáo: object **chỉ có trên DB**, **chỉ có trên Git**, **khác nội dung** (kèm diff).
4. Chạy theo lịch (scheduled pipeline hoặc cron trong server, mặc định mỗi đêm) và theo yêu cầu (`ovc drift`).
5. Có drift → gửi thông báo (email/webhook chat) cho maintainer của schema, ghi audit.
6. **Object còn là stub:** chưa có nội dung để so, nên drift check chỉ so danh sách object và `last_ddl_time` với giá trị server lưu lúc init env (§12). Khác → báo "đã đổi trên DB kể từ init (chưa xác minh nội dung)". Đây là tín hiệu yếu: biên dịch lại (kể cả do object phụ thuộc bị invalid) cũng làm đổi `last_ddl_time`. Nội dung chỉ được so chính xác sau khi object được hydrate.
7. Nếu `sync.auto_sync: true` trong `ovc.yaml` → tự chạy DB sync (§9.7) sau khi báo cáo.

### 9.7 DB sync (`ovc sync --env prd`)

Dùng khi DB bị sửa ngoài luồng (thường là sửa nóng lúc sự cố) và cần đưa Git về khớp với DB. Hoạt động như **"sync fork"**: lấy thay đổi từ DB, merge vào branch của env. OVC Server vẫn **chỉ đọc DB** – chỉ ghi lên GitLab.

**Không ghi đè thẳng đầu branch bằng DB**: branch có thể chứa commit đã merge nhưng chưa deploy, ghi đè sẽ âm thầm xoá chúng. Thay vào đó merge 3 chiều:

| Vai trò | Nội dung |
|---|---|
| **base** | Last deployed SHA của env (từ `OVC_DEPLOY_LOG`) – trạng thái DB *đáng lẽ* phải có. |
| **ours** | Trạng thái DB hiện tại (export) – chứa thay đổi ngoài luồng. |
| **theirs** | Đầu branch của env – chứa các commit mới chưa deploy (nếu có). |

```
 base (last deployed) ──●───────────●── theirs (đầu branch, có commit chưa deploy)
                         \           \
                          ●───────────●  merge commit → push lên branch
                    DB-state commit
                    (OVC-Sync: env=prd)
```

Các bước:

1. Kiểm tra env nằm trong `sync.envs`. Khoá branch của env (cùng lock với push).
2. Đọc last deployed SHA của env. Chưa từng deploy → từ chối (cần `--baseline` trước).
3. Export DDL từ DB như §9.1 bước 3–5 (chỉ các object trong `--object` nếu có). Chuẩn hoá **giống hệt** lúc init để không sinh khác biệt giả.
4. Tạo **DB-state commit** có cha là base, chứa đúng trạng thái DB:
   - Object khác nội dung → ghi đè file.
   - Object chỉ có trên DB → thêm file.
   - Object chỉ có trên Git → xoá file (object đã bị drop trên DB).
   - Không có khác biệt → kết thúc, trả `no_change`.
   - Message: `sync from DB <env>` + danh sách object; ghi thêm `LAST_DDL_TIME` của object trên DB (không biết ai sửa).
   - Trailer: `OVC-Sync: env=<env>; base=<base_sha>`. Author: service account OVC; `Co-authored-by` là người chạy lệnh.
5. Merge DB-state commit vào đầu branch:
   - Đầu branch = base → fast-forward.
   - Merge sạch → tạo merge commit, message mang cùng trailer `OVC-Sync` (để pipeline của commit đầu branch nhận ra).
   - Push lên branch bằng **service account OVC** (được phép push vào protected branch, chỉ dùng cho DB sync).
   - **Có conflict** (cùng object vừa bị sửa trên DB vừa có commit mới chưa deploy): **không tự giải**. Push DB-state commit lên branch `sync/<env>/<yyyyMMdd-HHmm>`, tạo MR vào branch của env, trả link MR (exit code 5). Dev xử lý bằng git: `git checkout sync/...` → `git merge origin/<branch>` → giải conflict → `git push`, rồi merge MR.
6. Pipeline nhận commit có trailer `OVC-Sync` → job `record-sync` ghi DB-state commit vào `OVC_DEPLOY_LOG` với `STATUS = SYNCED` (**không chạy DDL**, vì DB đã ở đúng trạng thái đó). Lần deploy tiếp theo tính diff từ DB-state commit → chỉ deploy các commit chưa deploy thật sự.
7. Lan sang branch khác: sync ở `prd` → server tự tạo PR/MR về `dev`, giống luồng hotfix, để các env khác cũng nhận thay đổi.
8. Ghi audit, gửi thông báo cho maintainer.

**Bảng / index / constraint / sequence:** DB sync cập nhật được **snapshot** (`tables/`, `indexes/`...) nhưng không sinh được `ALTER` (ngoài phạm vi §1.3). Khi phát hiện thay đổi cấu trúc:

- Commit/MR sync được gắn nhãn `needs-migration`, liệt kê object cấu trúc bị đổi.
- Dev viết migration tương ứng trong MR lan sang các branch thấp hơn (để dev có thay đổi).
- Ở env đã bị sửa tay, migration đó phải được đánh dấu đã chạy: `ovc deploy --env <env> --mark-applied <migration>` (job manual trong pipeline), nếu không deploy sẽ lỗi do chạy lại.

**Ngược lại – thay đổi trên DB là sai, muốn bỏ:** không sync, mà deploy lại từ Git để đè (code object chạy lại `CREATE OR REPLACE`; cấu trúc bảng cần migration đảo ngược viết tay).

### 9.8 Lấy nội dung và đồng bộ với DB (`ovc get`)

Mỗi lần `ovc get`, server **làm cho remote khớp DB trước** rồi CLI mới đưa về local. Server **chỉ đọc DB**, chỉ ghi lên Git host.

**Base của một object** là lần cuối remote và DB khớp nhau: hash của DDL và commit tương ứng. Server ghi base mỗi khi hydrate hoặc sync object đó (§12 `object_base`). Khi đã có pipeline deploy, base là commit deploy thành công gần nhất (`OVC_DEPLOY_LOG`).

1. CLI (chạy trong git working copy) xác định file cần lấy từ tham số: đường dẫn file, thư mục (mọi file trong đó), `OWNER.TÊN` hoặc `TÊN` (tìm trong mọi thư mục owner; trùng tên ở nhiều owner → yêu cầu ghi rõ owner). CLI `git fetch` branch DB rồi gửi **mọi** file đó lên server theo từng owner, kể cả file đã có nội dung.
2. Server đọc từ **DB của branch** (thường `dev`; user `OVC_READER`) chỉ các object được yêu cầu (riêng `ovc get <OWNER>` là cả owner), bằng `DBMS_METADATA.GET_DDL` với transform:
   - `SEGMENT_ATTRIBUTES = FALSE`, `STORAGE = FALSE`, `TABLESPACE = FALSE`
   - `EMIT_SCHEMA = FALSE`, `SQLTERMINATOR = TRUE`, `PRETTY = TRUE`
   - `CONSTRAINTS_AS_ALTER = TRUE`, `REF_CONSTRAINTS = FALSE` (FK lấy riêng vào `constraints/`)
   Sau đó chuẩn hoá: LF, bỏ khoảng trắng cuối dòng, chuẩn `CREATE OR REPLACE`, kết thúc `/`. Với bảng: kéo kèm index và constraint của bảng đó.
3. Với từng object, so ba phía **base / DB / remote** (đầu branch):

   | Tình huống | Server làm gì |
   |---|---|
   | Còn là vỏ trên remote | **Hydrate**: thay vỏ bằng DDL của DB. So `last_ddl_time` với lúc init; khác → vẫn hydrate, kèm cảnh báo. |
   | DB = base | Không làm gì. Remote khác base thì chỉ là code mới chưa deploy, **không được ghi đè**. |
   | DB ≠ base, remote = base | **Sync**: có người sửa tay trên DB → thay nội dung trên remote bằng DDL của DB. |
   | DB ≠ base, remote = DB | Hai phía đã giống nhau → chỉ cập nhật base. |
   | DB ≠ base, remote ≠ base, remote ≠ DB | **Conflict**: vừa sửa tay trên DB vừa có code mới chưa deploy → bước 5. |
   | Chưa có base (nội dung không do OVC ghi) và DB ≠ remote | Không biết bên nào mới hơn → coi là **conflict**. |
   | Có trên remote nhưng không còn trên DB | Chỉ cảnh báo (có thể chưa deploy, hoặc bị drop tay; xem drift §9.6). |

4. Hydrate và sync gộp vào **một commit trên branch của DB đó** (tác giả: người chạy lệnh; committer: service account; trailer `OVC-Hydrate` và/hoặc `OVC-Sync`), push bằng service account, rồi ghi base mới cho các object đó. Pipeline nhận commit có trailer này → job `record-sync` chỉ ghi nhận đã deploy (**không chạy DDL**, vì DB đã đúng nội dung này).
   - Không hydrate/sync đồng thời lên branch DB khác: nếu cả hai phía cùng thay vỏ bằng nội dung, rồi dev sửa tiếp, thì merge `dev` → `prd` luôn xung đột.
   - Hệ quả: lần đầu một object được đưa lên DB khác, PR/MR hiện cả file (vỏ → nội dung). Nếu nội dung trên DB đích đang lệch thì deploy sẽ ghi đè; drift check (§9.6) báo trước.
5. **Conflict giữa remote và DB: không tự giải.** Với mỗi object conflict:
   - Server tạo branch `sync/<db>/<OWNER>/<file>-<hash>-<thời điểm>` **từ commit base** của object, với một commit thay file bằng DDL hiện tại của DB, push, và mở PR/MR vào branch DB. Git host hiện conflict đúng ở object đó.
   - **Người giải quyết:** tác giả các commit chưa deploy của object (`git log <base>..<branch> -- <file>`), ghi trong mô tả MR kèm người chạy `ovc get`; maintainer của schema theo dõi. Họ gộp bằng git (giữ phần DB, phần code mới, hoặc cả hai), review, merge; branch sync bị xoá khi merge (bật tự xoá source branch trên Git host).
   - Còn branch `sync/<db>/<OWNER>/<file>-<hash>-*` của object đó → không tạo thêm, chỉ báo lại branch đang chờ.
   - **Chặn chỉ object trùng:** khi còn branch sync của một object, pipeline `validate` từ chối MR sửa object đó (§9.4) và job deploy dừng nếu lần deploy chứa object đó (§10.4). Các object khác deploy bình thường.
6. CLI `git fetch` lại, rồi đưa `origin/<branch>` vào branch hiện tại: đã chứa sẵn → không làm gì; branch hiện tại chưa có commit riêng → `git merge --ff-only`; có commit riêng (feature branch) → `git merge --no-edit` (như `git pull`). Git tự từ chối nếu thay đổi chưa commit của dev bị ghi đè; khi đó, hoặc khi merge conflict, CLI báo lỗi (exit 5) và để dev xử lý bằng git. `--no-merge`: chỉ fetch.
   - Có object conflict ở bước 5: CLI vẫn đưa phần còn lại về local; object conflict giữ **bản trên remote** (chưa có phần sửa tay trên DB). CLI in link MR sync, nhắc chờ MR đó trước khi sửa object, và thoát với mã 5.
7. Ghi audit: user, object, hành động (hydrate/sync/conflict), cảnh báo.

**Hai tầng conflict:**
- **Remote ↔ DB** (bước 5): xử lý trên Git host qua MR sync, người phụ trách là tác giả thay đổi chưa deploy.
- **Branch của dev ↔ remote**: conflict git bình thường khi merge `origin/<branch>`, dev tự giải. Dev không bao giờ conflict trực tiếp với DB: thay đổi trên DB chỉ đến máy dev sau khi đã thành commit trên remote.

Sau đó dev sửa file bình thường và đi tiếp luồng git: branch → push → PR/MR → merge → deploy (§9.3–9.5).

---

## 10. Deploy pipeline (GitHub Actions / GitLab CI)

### 10.1 Ánh xạ branch → môi trường

| Branch | DB | Kích hoạt |
|---|---|---|
| `dev` | `dev` | Tự động khi merge |
| `prd` | `prd` | **Cần duyệt tay** – chỉ nhóm được phép |

- `ovc deploy` **suy ra DB từ tên branch**, và **schema cần deploy từ các thư mục `<OWNER>/` có file thay đổi** kể từ lần deploy gần nhất của từng schema; mỗi thư mục được đối chiếu với `schema:` trong `<OWNER>/ovc.yaml`, không khớp thì dừng.
- Nhiều schema cùng thay đổi trong một lần merge: deploy lần lượt từng schema theo thứ tự cấu hình (`deploy_order` ở gốc branch, mặc định theo tên), mỗi schema dùng account deploy của nó (§10.3). Một schema lỗi → dừng, các schema sau không chạy.
- Đưa thay đổi lên prd: PR/MR từ `dev` vào `prd`. Hotfix: tạo feature branch từ `prd`, PR/MR vào `prd`, sau đó PR/MR `prd` → `dev` (cùng cơ chế back-merge của §9.7).
- Tên branch là khoá trong `databases` của cấu hình server (§13.1).

### 10.2 Template pipeline

File CI nằm **ở gốc từng DB branch** (tạo khi `ovc init` tạo branch) và chỉ gọi một template chung. Template chung đặt ở nơi khác để sửa một chỗ áp dụng cho mọi DB.

**GitHub (giai đoạn đầu)** – workflow trong branch gọi reusable workflow:

```yaml
# .github/workflows/ovc-deploy.yml (ở gốc mỗi DB branch)
name: ovc-deploy
on:
  push:
    branches: ["dev", "prd"]
  pull_request:
    branches: ["dev", "prd"]
jobs:
  ovc:
    uses: <org>/ovc-ci-templates/.github/workflows/oracle-deploy.yml@v1
    secrets: inherit
```

```yaml
# <org>/ovc-ci-templates/.github/workflows/oracle-deploy.yml
on:
  workflow_call:
concurrency:
  group: ovc-${{ github.ref_name }}      # mỗi DB chỉ 1 deploy tại 1 thời điểm
  cancel-in-progress: false
jobs:
  validate:
    if: github.event_name == 'pull_request'
    runs-on: [self-hosted, ovc]
    container: registry.company.local/ovc/ovc-cli:1
    steps:
      - uses: actions/checkout@v4
      - run: ovc deploy --dry-run --db "$GITHUB_BASE_REF"
  record-sync:        # commit DB sync / hydrate: chỉ ghi OVC_DEPLOY_LOG, không chạy DDL
    if: github.event_name == 'push' && (contains(github.event.head_commit.message, 'OVC-Sync') || contains(github.event.head_commit.message, 'OVC-Hydrate'))
    runs-on: [self-hosted, ovc]
    container: registry.company.local/ovc/ovc-cli:1
    environment: ${{ github.ref_name }}
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - run: ovc deploy --record-sync
        env: { OVC_SECRETS: "${{ toJSON(secrets) }}" }   # OVC_DB_DSN + OVC_DB_USER_<OWNER>/OVC_DB_PASSWORD_<OWNER>
  deploy:
    if: github.event_name == 'push' && !contains(github.event.head_commit.message, 'OVC-Sync') && !contains(github.event.head_commit.message, 'OVC-Hydrate')
    runs-on: [self-hosted, ovc]
    container: registry.company.local/ovc/ovc-cli:1
    environment: ${{ github.ref_name }}  # prd: cấu hình "required reviewers" trên environment = duyệt tay
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - run: ovc deploy
        env: { OVC_SECRETS: "${{ toJSON(secrets) }}" }   # OVC_DB_DSN + OVC_DB_USER_<OWNER>/OVC_DB_PASSWORD_<OWNER>
```

**GitLab (sau khi chuyển)** – cùng ý tưởng:

```yaml
# .gitlab-ci.yml (ở gốc mỗi DB branch)
include:
  - project: db/ovc-ci-templates
    file: /oracle-deploy.yml
```

Template GitLab dùng `environment: $CI_COMMIT_BRANCH`, `resource_group: $CI_COMMIT_BRANCH`, `when: manual` cho branch `prd`, và job `record-sync` chạy khi message có `OVC-Sync` hoặc `OVC-Hydrate`, tương đương hai job ở trên.

Điểm cần hiểu:
- `ovc deploy` là cùng một binary ở cả hai host; chỉ file CI khác nhau. Chuyển host = đổi remote của OVC Server, đưa file CI GitLab vào các branch (hoặc để `ovc init` sinh cả hai ngay từ đầu), và dùng driver tạo PR/MR của GitLab (§4.3).
- Runner phải chạy trong mạng nội bộ để tới được Oracle (GitHub: self-hosted runner).

### 10.3 Credential deploy

- **Account deploy theo dự án:** mỗi dự án/schema xin cấp một account có đủ quyền trên schema mình dùng (account do DBA cấp sau, theo yêu cầu của dự án). Không có account dùng chung cho mọi schema.
- Credential đặt trong **secret gắn với environment** (GitHub: Environment secrets; GitLab: biến có environment scope), **một environment cho mỗi DB branch** (`dev`, `prd`):
  - `OVC_DB_DSN` của DB; với mỗi schema `OVC_DB_USER_<OWNER>`, `OVC_DB_PASSWORD_<OWNER>` (vd `OVC_DB_USER_HR`). `ovc deploy` chỉ dùng account của schema đang deploy.
  - Job `deploy` khai báo `environment: <tên branch>` nên chỉ đọc được secret của DB đó.
  - **Đánh đổi** của mô hình một branch một DB: pipeline của branch `dev` đọc được mật khẩu của mọi schema trên DB dev. Ai sửa được file CI/pipeline của branch là có thể lấy chúng, nên file CI phải được bảo vệ như code (review bắt buộc, CODEOWNERS cho `.github/`, `.gitlab-ci.yml`).
- Environment `prd`: bật **bước duyệt tay** (GitHub: required reviewers; GitLab: protected environment + `when: manual`) và giới hạn chỉ deploy được từ branch `prd`.
- **Kết nối trực tiếp** bằng account trên, không dùng proxy authentication (driver `go-ora` chưa hỗ trợ). Nếu account không phải owner của schema, `ovc deploy` chạy `ALTER SESSION SET CURRENT_SCHEMA = <schema trong ovc.yaml>` ngay sau khi kết nối để object tạo đúng owner; vì DDL không ghi tên schema (§5.2) nên hoạt động đúng.
- Account deploy cần quyền ghi các bảng `OVC_ADMIN` (§10.5); `OVC_READER` cần quyền `SELECT` trên các bảng đó để drift/DB sync đọc `OVC_DEPLOY_LOG`.

### 10.4 Thuật toán `ovc deploy`

0. Xác định các schema cần deploy: thư mục `<OWNER>/` có file đổi so với commit đã deploy gần nhất của schema đó. Các bước dưới chạy **cho từng schema**, theo thứ tự §10.1, chỉ xét file trong `<OWNER>/`.
1. Kết nối DB bằng account deploy của schema (§10.3), đọc commit đã deploy gần nhất từ `OVC_ADMIN.OVC_DEPLOY_LOG` (schema + env, `STATUS IN ('SUCCESS','SYNCED')`).
   - Quét `git log <last_sha>..<CI_COMMIT_SHA>` tìm DB-state commit có trailer `OVC-Sync: env=<env>; base=<last_sha>` hoặc commit hydrate có trailer `OVC-Hydrate`. Có → ghi nhận nó là đã deploy (`SYNCED`) và dùng làm `last_sha` (cùng logic với `--record-sync`, idempotent – xử lý cả trường hợp sync qua MR khi conflict, vì merge commit của MR không có trailer).
   - `--record-sync` dừng ở bước này.
2. Tính danh sách file thay đổi: `git diff --name-status <last_sha> <CI_COMMIT_SHA>`. **Bỏ qua file stub** (dòng `-- OVC:STUB`, §5.5): không deploy và không tính là file bị xoá/sửa. Chưa từng deploy → yêu cầu `--bootstrap` hoặc `--baseline` (đánh dấu commit hiện tại là đã deploy mà không chạy gì – dùng cho DB đã có sẵn code lúc init).
3. **Chặn object đang có MR sync:** fetch `refs/heads/sync/<db>/*`; nếu file sắp deploy trùng với file mà một branch sync còn tồn tại đang sửa → dừng trước khi chạy DDL nào, in tên branch/MR sync. Chỉ chặn object trùng; lần deploy không đụng các object đó chạy bình thường.
4. Ghi bản ghi deploy trạng thái `RUNNING`, lấy lock (`DBMS_LOCK`) theo schema để chặn deploy song song.
5. Chạy **migration** chưa có trong `OVC_MIGRATION_LOG`, theo thứ tự tên; mỗi migration thành công ghi log ngay. Lỗi → dừng.
6. Chạy **code object** đã thêm/sửa theo thứ tự phụ thuộc:
   1. sequences, synonyms
   2. type spec
   3. package spec
   4. function, procedure
   5. view, materialized view
   6. type body, package body
   7. trigger
   8. grants
7. File code object bị xoá: nếu `allow_drop: true` sinh `DROP`, ngược lại **fail** và yêu cầu migration drop tường minh.
8. Compile lại object invalid (`DBMS_UTILITY.COMPILE_SCHEMA(schema, compile_all => FALSE)`).
9. So danh sách object invalid trước/sau deploy; phát sinh invalid mới và `fail_on_new_invalid: true` → pipeline **fail** (exit 6), in lỗi từ `ALL_ERRORS`.
10. Cập nhật `OVC_DEPLOY_LOG` (`SUCCESS`/`FAILED`, commit, pipeline id, người merge, thời gian, danh sách file).
11. Gửi kết quả về OVC Server (webhook, tuỳ chọn) để lưu audit và comment vào MR.

> Lưu ý: DDL trên Oracle tự commit, không rollback được theo transaction. Deploy lỗi giữa chừng → sửa và deploy tiếp (roll-forward). Code object có thể quay về bằng cách deploy lại commit cũ; migration cần script đảo ngược viết tay.

### 10.5 Bảng quản trị trên mỗi DB đích (schema `OVC_ADMIN`)

```sql
CREATE TABLE OVC_DEPLOY_LOG (
  ID            NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  SCHEMA_NAME   VARCHAR2(128) NOT NULL,
  ENV           VARCHAR2(30)  NOT NULL,
  COMMIT_SHA    VARCHAR2(40)  NOT NULL,
  PREV_SHA      VARCHAR2(40),
  STATUS        VARCHAR2(20)  NOT NULL,      -- RUNNING / SUCCESS / FAILED / SYNCED (DB sync, không chạy DDL)
  PIPELINE_ID   VARCHAR2(50),
  TRIGGERED_BY  VARCHAR2(128),
  STARTED_AT    TIMESTAMP DEFAULT SYSTIMESTAMP,
  FINISHED_AT   TIMESTAMP,
  DETAIL        CLOB                          -- JSON: file đã chạy, lỗi
);

CREATE TABLE OVC_MIGRATION_LOG (
  SCHEMA_NAME   VARCHAR2(128) NOT NULL,
  MIGRATION     VARCHAR2(255) NOT NULL,
  CHECKSUM      VARCHAR2(64)  NOT NULL,       -- phát hiện migration bị sửa sau khi chạy
  DEPLOY_ID     NUMBER REFERENCES OVC_DEPLOY_LOG(ID),
  APPLIED_AT    TIMESTAMP DEFAULT SYSTIMESTAMP,
  CONSTRAINT PK_OVC_MIGRATION_LOG PRIMARY KEY (SCHEMA_NAME, MIGRATION)
);
```

---

## 11. Bảo mật & bắt buộc "một luồng"

### 11.1 User Oracle

| User | Quyền | Ai giữ |
|---|---|---|
| `OVC_READER` | `CREATE SESSION`, `SELECT_CATALOG_ROLE`, `SELECT ANY DICTIONARY` (đọc `DBA_OBJECTS`, `DBMS_METADATA`) | OVC Server |
| Account deploy (theo dự án) | Quyền DDL trên schema của dự án + quyền ghi `OVC_ADMIN`; do DBA cấp khi dự án yêu cầu | Secret của environment CI của DB (`OVC_DB_USER_<OWNER>` trong environment `dev`/`prd`) |
| User của dev | Không được cấp quyền DDL trên schema ở env dùng chung (theo quy ước) | Dev |

### 11.2 Giữ kỷ luật "một luồng" (không dùng trigger)

Không cài trigger DDL trên DB. Việc không sửa tay là **quy ước của nhóm**, được hỗ trợ bởi:

- **Drift check** hằng đêm (§9.6): phát hiện object bị sửa ngoài pipeline và báo cho maintainer.
- **DB sync** (§9.7): khi lỡ có sửa tay thì đưa về Git ngay, không để Git lệch với DB.
- **Hạn chế cấp account** có quyền DDL: dev không được cấp account deploy; account deploy chỉ nằm trong CI variables của project.
- Hệ quả chấp nhận: không biết *ai* sửa tay (không có audit DDL phía DB). Nếu sau này cần thì có thể bật Oracle Unified Auditing hoặc trigger quan sát, ngoài phạm vi hiện tại.

### 11.3 Git host

- Bảo vệ branch `dev`, `prd` (cài một lần cho cả repo): chỉ thay đổi qua PR/MR. **Ngoại lệ duy nhất**: service account OVC được phép tạo và push các branch này (bypass) để ghi baseline lúc init, commit DB sync (§9.7) và hydrate (§9.8); các commit này có trailer `OVC-Init`, `OVC-Sync` hoặc `OVC-Hydrate` và được audit. Dev push được feature branch bằng tài khoản của mình, không push được lên `dev`, `prd`.
- Bật "pipeline phải pass" trước khi merge, để kiểm tra của §9.4 (sửa file vỏ, sửa migration cũ...) là bắt buộc.
- Service account Git host của OVC: quyền ghi vào repo (tạo branch, push, tạo PR/MR) và được bypass quy tắc bảo vệ branch như trên. GitHub: nên dùng GitHub App hoặc tài khoản bot với fine-grained token chỉ cấp cho repo này. Token lưu ở biến môi trường/secret manager của OVC Server.
- PR/MR cần tối thiểu 1 approval (cấu hình bằng branch protection / approval rule).
- Deploy `prd` cần duyệt tay trên environment `prd` (GitHub: required reviewers; GitLab: protected environment).

### 11.4 OVC Server

- Chỉ chạy HTTPS (TLS do reverse proxy nội bộ cấp).
- **Không xác thực người gọi** (bản đầu): đặt OVC Server sau mạng nội bộ/VPN, chỉ cho dải IP của dev truy cập. Hệ quả cần hiểu: ai tới được server đều có thể gọi `init`, `get`, `sync`; hai thao tác sau ghi thẳng vào branch bảo vệ bằng service account (không qua MR). Mọi lời gọi đều được audit kèm IP và danh tính khai báo.
- Credential `OVC_READER` và service token GitLab (dùng cho init/drift/DB sync) lấy từ biến môi trường / secret manager, không ghi trong file cấu hình.
- Audit mọi thao tác ghi (init, push, MR, drift, DB sync).
- Rate limit theo IP.

---

## 12. Metadata DB của OVC Server

PostgreSQL, các bảng chính:

| Bảng | Nội dung |
|---|---|
| `db_alias` | Alias DB (`dev`, `prd`): DSN, tham chiếu secret của `OVC_READER`. |
| `schema_registry` | Schema (owner) đã đăng ký: tên, SHA owner baseline, ngày init đầu tiên. (Repo chỉ có một, cấu hình ở §13.1.) |
| `schema_db` | Mỗi DB đã init của schema: alias DB (= branch), commit init, số object, người/ngày init. |
| `stub_ddl_time` | `last_ddl_time` lúc init của từng object, theo schema + DB (dùng cho §9.6 bước 6, §9.8 bước 3). |
| `object_base` | Base của từng object theo schema + DB: hash DDL và commit lúc remote và DB khớp nhau lần cuối (§9.8). |
| `job` | Job bất đồng bộ (init, drift, DB sync): trạng thái, tiến độ, kết quả. |
| `drift_report` | Kết quả drift check theo lần chạy. |
| `sync_run` | Lần chạy DB sync: env, base SHA, DB-state commit, kết quả (`synced`/`conflict`/`no_change`), link MR, danh sách object, cờ `needs_migration`. |
| `audit_log` | danh tính khai báo (tên/email), IP, hành động, schema, branch, commit, thời gian, IP, chi tiết JSON. |

---

## 13. Cấu hình

### 13.1 Server (`ovc-server.yaml` + biến môi trường)

```yaml
listen: ":8080"
public_url: https://ovc.company.local
git:
  host: github                          # github | gitlab (đổi khi chuyển sang GitLab)
  remote: https://github.com/<org>/<repo>.git
  api_url: https://api.github.com       # GitLab: https://gitlab.company.local/api/v4
  repo: <org>/<repo>                    # GitLab: <group>/<project>
  token_env: OVC_GIT_TOKEN              # token của service account
storage:
  mirror_dir: /var/lib/ovc/repos
metadata_db:
  dsn_env: OVC_METADATA_DSN
databases:
  dev:  { dsn: "dbdev.company.local:1521/DEVPDB",  user: OVC_READER, password_env: OVC_DB_DEV_PASSWORD }
  prd: { dsn: "dbprod.company.local:1521/PRODPDB", user: OVC_READER, password_env: OVC_DB_PRD_PASSWORD }
export:
  workers: 8
drift:
  schedule: "0 1 * * *"
cli:
  min_version: 0.1.0
  download_dir: /var/lib/ovc/cli
```

### 13.2 CLI

CLI không có file cấu hình:
- **Địa chỉ OVC Server** do OVC quyết định, gắn vào binary lúc build: `make dist SERVER=https://ovc.company.local` (`-ldflags -X ovc/internal/cli.DefaultServer=...`). Dev tải CLI đã build sẵn (hoặc `ovc update` từ server). Biến môi trường `OVC_SERVER` chỉ dùng để ghi đè khi test.
- **Danh tính** (tác giả commit hydrate/sync, audit): `git config user.name` / `user.email` của working copy, như commit git của chính dev. Không xác thực (§8).

---

## 14. Công nghệ

| Hạng mục | Lựa chọn |
|---|---|
| Ngôn ngữ | Go ≥ 1.23 |
| CLI framework | `github.com/spf13/cobra` |
| HTTP server | `net/http` + `github.com/go-chi/chi/v5` |
| Oracle driver | `github.com/sijms/go-ora/v2` (thuần Go, không cần Instant Client) |
| Git host | `git` (CLI) cho mọi thao tác Git; `github.com/google/go-github` (GitHub) và `gitlab.com/gitlab-org/api/client-go` (GitLab) chỉ để tạo PR/MR |
| Git trên server | Gọi lệnh `git` (≥ 2.40) – đầy đủ merge/rebase hơn thư viện thuần Go |
| Metadata DB | PostgreSQL 15+ (`github.com/jackc/pgx/v5`) |
| Cấu hình | YAML (`gopkg.in/yaml.v3`) + biến môi trường |
| Log | `log/slog` (JSON) |
| Đóng gói | Binary cho `windows/amd64`, `linux/amd64`, `darwin/arm64`, `darwin/amd64`; Docker image cho server và CLI (dùng trong CI) |

---

## 15. Cấu trúc source của dự án OVC

```
oracle_version_control/
├── cmd/
│   ├── ovc/                  # main của CLI
│   └── ovc-server/           # main của server
├── internal/
│   ├── cli/                  # các lệnh cobra
│   ├── client/               # HTTP client gọi OVC Server
│   ├── gitlocal/             # gọi git trên máy dev cho ovc get (fetch, đọc remote, merge)
│   ├── server/               # HTTP handlers, middleware auth
│   ├── gitops/               # mirror, merge 3 chiều, commit, push
│   ├── githost/              # interface tạo PR/MR + bản cài đặt github, gitlab
│   ├── oracle/
│   │   ├── export/           # DBMS_METADATA, chuẩn hoá DDL
│   │   ├── deploy/           # thuật toán deploy, thứ tự, recompile
│   │   ├── drift/
│   │   └── dbsync/           # DB sync: DB-state commit, merge 3 chiều, MR lan branch
│   ├── layout/               # quy tắc thư mục/đuôi file/tên object
│   ├── store/                # metadata DB (PostgreSQL)
│   └── audit/
├── sql/
│   └── install/              # script tạo OVC_ADMIN, bảng log, user OVC_READER
├── ci-templates/             # workflow GitHub Actions + template GitLab CI dùng chung
├── deploy/                   # Dockerfile, docker-compose
├── docs/
└── specs.md
```

---

## 16. Yêu cầu phi chức năng

| Hạng mục | Yêu cầu |
|---|---|
| Hiệu năng | Init schema 5.000 object (chỉ vỏ) < 1 phút; hydrate 20 object < 10 giây; clone < 30 giây; push < 5 giây (không tính thời gian mạng GitLab). Export đầy đủ (`init --full`) 5.000 object < 10 phút nếu cần. |
| Đồng thời | Hỗ trợ ≥ 50 dev dùng cùng lúc; khoá theo branch khi push. |
| Sẵn sàng | Server không có trạng thái quan trọng ngoài metadata DB và mirror (mirror dựng lại được từ GitLab). |
| Sao lưu | Backup metadata DB hằng ngày. Source chính nằm ở GitLab (đã có backup của công ty). |
| Dự phòng | OVC ngừng hoạt động → dev vẫn có thể dùng git trực tiếp với GitLab; pipeline deploy không phụ thuộc OVC Server. |
| Quan sát | Log JSON, endpoint `/metrics` (Prometheus), audit log tra cứu được. |
| Tương thích | Oracle 19c; GitHub (Actions) và GitLab ≥ 16; Windows 10+, macOS 12+, Linux. |

---

## 17. Lộ trình

| Phase | Nội dung | Kết quả |
|---|---|---|
| **0 – Nền tảng** | Khung repo Go, CI build binary, script `sql/install` (OVC_ADMIN, user, bảng log). | Build được `ovc` và `ovc-server`. |
| **1 – Init (vỏ) & Hydrate** | `oracle/export` (liệt kê object + `GET_DDL` theo yêu cầu), `ovc init` thêm thư mục schema (bộ file vỏ) vào DB branch trong repo có sẵn, `ovc get`, `internal/gitops` (mirror, commit, push). | Mỗi DB là một branch, mỗi schema một thư mục; kéo nội dung khi cần. |
| **2 – Deploy pipeline** | `ovc deploy` (migration + code object + recompile + log), CI template. | Merge vào branch → tự deploy. **Từ đây đã có "một luồng".** |
| **3 – Workspace CLI** | `login`, `clone`, `status`, `diff`, `restore`, `pull`, `push`, `branch`, `mr`. | Dev làm việc qua OVC thay vì sửa trên DB. |
| **4 – Drift & sync** | Drift check định kỳ + báo cáo; `ovc sync` (DB → Git, job `record-sync`, `--mark-applied`). | Phát hiện và đồng bộ ngược thay đổi ngoài luồng. |
| **5 – Hoàn thiện** | `ovc update`, audit UI/tra cứu, comment kết quả vào MR, metrics, ký số binary Windows. | Sẵn sàng dùng rộng. |

> Phase 1–2 đã đủ để dev dùng **git trực tiếp** với GitLab nếu cần; phase 3 mang lại trải nghiệm qua OVC.

---

## 18. Vấn đề còn mở

1. **Test trước khi push:** dev không có quyền DDL → cần một schema/PDB **sandbox** cho từng dev, hay chấp nhận kiểm tra tại env `dev` sau khi merge vào `dev`?
2. **Kiểm tra compile trong MR:** có cấp một schema "scratch" để pipeline `validate` compile thử code trước khi merge không?
3. **Phụ thuộc giữa các schema:** các schema của một DB nằm chung branch và chung pipeline, deploy theo `deploy_order` (§10.1). Còn cần quyết định: thứ tự khai báo tay hay suy ra từ `DBA_DEPENDENCIES`; và một schema deploy lỗi thì có chặn các schema không phụ thuộc nó không.
4. **Object ngoài phạm vi:** DB link, scheduler job, queue (AQ), context, policy (VPD) – quản lý hay loại trừ?
5. **Dữ liệu master/cấu hình:** có cần version control cho một số bảng tham số không?
6. **Xác thực người gọi OVC Server:** bản đầu không có, dựa vào mạng nội bộ. Cần cân nhắc ít nhất một khoá dùng chung cho các thao tác ghi thẳng vào branch bảo vệ (`init`, `sync`, `get`) để không ai tới được server cũng gọi được; về sau có thể thêm tài khoản riêng/LDAP nếu cần biết chính xác ai làm gì.
7. **Account deploy theo dự án:** quy trình xin cấp (ai duyệt, DBA cấp trong bao lâu), quyền tối thiểu cần có trên schema, và cách xoay mật khẩu trong CI variables.
8. **Hotfix:** có đi thẳng vào `prd` rồi back-merge về `dev` như §10.1 không? (Chỉ có hai env `dev` và `prd`, không có `test`.)
9. **DB sync / hydrate push thẳng vào branch được bảo vệ:** cần Git host cho service account bypass quy tắc bảo vệ (GitHub: ruleset bypass hoặc không bật "restrict pushes" cho account này; GitLab CE chỉ cho chọn theo vai trò nên mọi Maintainer cũng push được). Công ty có chấp nhận không, hay mọi DB sync đều phải qua PR/MR?
10. **`auto_sync`** có nên bật cho prd không, hay chỉ cho dev và prd luôn sync thủ công?
11. **`last_ddl_time` lúc init (lưu ở server):** biên dịch lại làm đổi `last_ddl_time` nên có thể báo "đã đổi" sai với object chưa hydrate. Có cần lưu thêm hash nội dung lúc init (tính bằng SQL từ `DBA_SOURCE`, rẻ hơn `GET_DDL`) để so chính xác không?
12. **Lần đầu đưa object lên env khác:** hydrate chỉ ghi branch của env đang làm, nên PR/MR `dev` → `prd` đầu tiên của một object hiện cả file thay vì chỉ phần sửa. Có cần thêm bước "hydrate env đích" (lấy nội dung từ DB prd, so với dev, chỉ cảnh báo nếu lệch) để reviewer thấy rõ phần ghi đè không?
13. **Secret theo environment:** cần xác nhận gói GitHub của công ty (và GitLab sau này) hỗ trợ environment secrets / environment-scoped variables cho repo này, vì §10.3 dựa vào đó để tách credential giữa các schema trong cùng repo. Nếu không, mọi pipeline đọc được mọi credential.
14. **Ghi đồng thời trên một DB branch:** mọi schema của một DB commit lên cùng branch nên hay đụng nhau hơn (push, hydrate, sync, PR merge). Server dùng compare-and-swap và tự làm lại khi thay đổi không chạm cùng file; cần đo khi có nhiều schema hoạt động cùng lúc. Feature branch vẫn cần quy ước dọn dẹp.
15. **Chuyển GitHub → GitLab:** ngoài đổi remote còn phải cài lại branch protection, environment/secret, runner và file CI trong các branch; nên có checklist và lệnh `ovc doctor` kiểm tra cấu hình host.
