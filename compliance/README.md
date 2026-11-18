# Configuration Compliance — FRR / Alpine Linux / Ansible / SQLite

Module kiểm tra cấu hình router theo mô hình **read-only**. Không có lệnh cấu hình, không ghi `write memory`, không tự sửa router.

## 1. Kiến trúc

- VM1 R1, VM2 R2, VM3 R3: Alpine Linux + FRRouting (FRR).
- VM4 Controller: runner (Go hoặc Python) + SQLite; Dashboard Go API đọc lịch sử từ cùng database.
- Runner SSH vào từng router, chạy `vtysh -c 'show running-config'` và lưu snapshot với quyền file hạn chế.
- Runner đối chiếu từng router với `config/desired_state.yml`.
- Mỗi lần audit được ghi vào `files/dashboard/db/net.db`, bảng `compliance_runs` và `compliance_findings`.
- Dashboard có mục **Compliance** và API `GET /api/compliance` (yêu cầu đăng nhập).

Có hai runner với hành vi **giống hệt nhau** (xem mục 7 để kiểm chứng):

| Runner | Thu thập cấu hình | Yêu cầu trên Controller |
|---|---|---|
| `compliance/go` (Go) | `--native`: SSH trực tiếp; hoặc gọi `ansible-playbook` | Không cần gì thêm khi dùng `--native` |
| `compliance/scripts/run_compliance.py` (Python) | `ansible-playbook` | Python 3 + PyYAML + Ansible |

`compliance/run.sh` tự chọn binary Go nếu đã build, nếu không thì dùng Python. Nhờ vậy quy trình cũ vẫn chạy nguyên vẹn.

## 2. Cài trên Alpine Linux Controller

Chạy từ thư mục gốc project:

```sh
sh compliance/install_alpine.sh          # Python + Ansible (như cũ)
sh compliance/install_alpine.sh --go     # chỉ Go, không cần Python/Ansible
```

Chế độ mặc định dùng `apk`, tạo Python virtual environment `.venv`, sau đó cài `ansible-core` và `PyYAML`.

Chế độ `--go` cài `go` + `openssh-client` rồi build binary vào `compliance/bin/compliance`. Đây là cách nhẹ nhất: Controller không cần Python, PyYAML hay Ansible. Có thể build thủ công ở bất kỳ đâu có Go:

```sh
make -C compliance/go build
```

Cả hai chế độ đều không cài thêm agent lên R1/R2/R3.

Nếu Controller không phải Alpine, cài OpenSSH client (và `sshpass` nếu dùng SSH password cho chế độ Python) bằng package manager phù hợp.

## 3. Chuẩn bị inventory và SSH

```sh
cp hosts.ini.example hosts.ini
```

Sửa `hosts.ini` để địa chỉ, username và phương thức xác thực đúng với lab. Không giữ `CHANGE_ME`, không commit mật khẩu thật. Ưu tiên SSH key hoặc Ansible Vault. Với password authentication ở chế độ Python, Controller cần `sshpass`; chế độ `--native` của Go không cần `sshpass`.

Kiểm tra kết nối trước khi audit:

```sh
.venv/bin/ansible -i hosts.ini routers -m ansible.builtin.ping   # chế độ Python
```

Kiểm tra FRR trên từng router:

```sh
ssh root@192.168.23.11 'vtysh -c "show running-config"'
```

Lặp lại với IP R2/R3. User SSH phải có quyền gọi `vtysh`.

## 4. Chạy Compliance

Từ thư mục gốc project:

```sh
./compliance/run.sh hosts.ini
```

Hoặc gọi trực tiếp một trong hai runner:

```sh
./compliance/bin/compliance --inventory hosts.ini --native   # Go, không cần Ansible
.venv/bin/python compliance/scripts/run_compliance.py --inventory hosts.ini
```

Kết quả được in ra màn hình và lưu tại:

- `compliance/reports/latest.json`
- `compliance/reports/actual/r1.conf` (tương tự r2/r3; quyền hạn chế)
- `files/dashboard/db/net.db` — lịch sử để Dashboard truy vấn.

Mã thoát: `0` = tất cả router compliant và thu thập thành công; `1` = có lỗi/non-compliant; `2` = lỗi đầu vào/cấu hình/database.

Chỉ dùng `--skip-collect` khi cố ý kiểm thử bằng các file `.conf` đã có:

```sh
./compliance/bin/compliance --skip-collect
```

Mặc định mỗi lần chạy sẽ xóa snapshot cũ trước khi thu thập. Router không SSH được sẽ thành `ERROR`, không được chấm nhầm bằng cấu hình từ lần chạy trước.

## 5. Dashboard

Nếu Dashboard đã được triển khai từ project này, sau khi cập nhật source:

```sh
cd files/dashboard
 docker compose up -d --build api
```

API `GET /api/compliance` trả về kết quả audit mới nhất, các rule và 20 lần chạy gần nhất. Dashboard chỉ đọc kết quả; thao tác chạy audit vẫn thực hiện trên Controller bằng lệnh ở mục 4.

Nếu API container đang dùng một file DB khác, cần bảo đảm biến `NETDB` của API và tham số `--db` của runner trỏ tới cùng một file SQLite. Cấu hình Compose hiện tại dùng `./db:/data` và `NETDB=/data/net.db`, tương ứng với `files/dashboard/db/net.db` trên host.

## 6. Desired state

Mở `compliance/config/desired_state.yml`. Mỗi router có `required_patterns` là danh sách regex được tìm trong output FRR `show running-config`. Có thể thêm/xóa rule theo cấu hình chuẩn của lab.

Các rule mẫu khớp với playbook OSPF hiện có: hostname, OSPF process, network LAN, loopback /32 và rule loopback-passive (xem dưới).

> **Lưu ý — còn một rule không khớp thực tế lab.**
>
> - `^\s*network 192\.168\.23\.0/24 area 0$` — prefix này đã được chuyển khỏi OSPF sang các link transit (`10.0.1.0/29`, `10.0.3.0/29`), nên router đúng chuẩn vẫn báo thiếu. Đây là **drift thật** giữa repo và thiết bị, cố ý giữ nguyên để không che mất khác biệt. Cần quyết định: khôi phục `/24` vào OSPF, hoặc chấp nhận thiết kế `/29` rồi sửa `02_ospf.yml`, `05_acl.yml` và file rule này cho đồng bộ.
>
> **Đã sửa — rule loopback-passive.** Trước đây rule là `^\s*passive-interface lo$`. FRR 10.6 vẫn nhận lệnh `passive-interface lo` (nên `02_ospf.yml` đúng), nhưng in trạng thái ra thành `ip ospf passive` bên trong block `interface lo`. Không cấu hình nào thỏa được rule cũ, nên lỗi nằm ở rule chứ không ở router. Rule hiện tại chấp nhận cả hai dạng:
>
> ```
> ^(?:[ \t]*passive-interface lo|interface lo\r?$\n(?:[ \t]+[^\n]*\r?\n)*?[ \t]+ip ospf passive)$
> ```
>
> Nhánh thứ hai bị giới hạn trong block `interface lo`: dòng `exit` không thụt lề nên không khớp `[ \t]+[...]`, vì vậy `ip ospf passive` dưới một interface khác **không** làm rule pass. Cả hai chiều được khóa bằng `TestPassivationRuleIsScopedToLoopback`. Xem `compliance/PORT-REPORT.md` §7.7–§7.8.

Regex được biên dịch với ngữ nghĩa Python `re.MULTILINE`: `^`/`$` khớp theo từng dòng. Bản Go mở rộng `\s`, `\S`, `\d`, `\D`, `\w`, `\W` thành character class tương đương Python (Unicode-aware), và chuẩn hóa `\r\n` về `\n` trước khi so khớp — nếu không, snapshot CRLF sẽ làm mọi rule neo `$` fail.

Regex được biên dịch với ngữ nghĩa Python `re.MULTILINE`: `^`/`$` khớp theo từng dòng. Bản Go mở rộng `\s`, `\S`, `\d`, `\D`, `\w`, `\W` thành character class tương đương Python (Unicode-aware), và chuẩn hóa `\r\n` về `\n` trước khi so khớp — nếu không, snapshot CRLF sẽ làm mọi rule neo `$` fail.

## 7. Kiểm thử

Python:

```sh
.venv/bin/python -m py_compile compliance/scripts/run_compliance.py
.venv/bin/python -m unittest discover -s compliance/tests -v
```

Go:

```sh
make -C compliance/go test          # unit test
make -C compliance/go check         # gofmt + go vet + test
```

Kiểm chứng hai runner cho ra kết quả giống nhau (cần Python + PyYAML):

```sh
make -C compliance/go parity
```

Script `compliance/tests/parity.py` chạy **cả hai** runner trên cùng fixture và cùng `desired_state.yml` thật, rồi so sánh stdout, `latest.json` và cả hai bảng SQLite. Đây là bằng chứng bản Go tương đương bản Python, không chỉ là "trông có vẻ đúng".

Có thể build lại API từ `files/dashboard` bằng Docker Compose. Cần chạy thử SSH/FRR thực tế trên VMware để xác nhận output đúng với phiên bản Alpine/FRR của lab; môi trường phát triển không có quyền truy cập các VM đó.

## An toàn và giới hạn

- Audit chỉ đọc `show running-config`; không có auto-remediation. Không có lệnh nào trong `collect_native.go` thay đổi trạng thái router.
- Snapshot cấu hình có thể chứa thông tin nhạy cảm. Thư mục report được giới hạn quyền truy cập; không gửi file `.conf` lên GitHub hoặc chia sẻ công khai.
- Dữ liệu compliance chỉ được thêm vào các bảng riêng `compliance_runs` và `compliance_findings`; không xóa/ghi đè bảng dashboard hiện có.
- Không chạy `playbooks/16_vlan.yml`, NAT/DHCP hoặc playbook khác như một phần của Compliance.
- Chế độ `--native` dùng `ssh.InsecureIgnoreHostKey()` vì lab tạo lại host key mỗi lần rebuild — cùng đánh đổi với `host_key_checking = False` của Ansible. Không dùng nguyên trạng cho môi trường production.
