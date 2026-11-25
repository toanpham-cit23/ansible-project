# Kịch bản demo cho giáo sư

Tài liệu này là thứ tự thao tác trên màn hình, kèm câu nói. Mỗi bước đều
có bằng chứng đo được, không phải "trông có vẻ đúng".

Thời lượng: **8–10 phút**. Đường dẫn: **http://192.168.23.10:8080**

---

## 0. Chuẩn bị trước khi giáo sư vào (2 phút, làm trước)

Mở sẵn ba tab, đăng nhập sẵn:

| Tab | Địa chỉ | Để làm gì |
|---|---|---|
| 1 | `http://192.168.23.10:8080/#/monitoring` | Trang Monitor |
| 2 | `http://192.168.23.10:8080/#/leases` | Trang Leases |
| 3 | `http://192.168.23.10:8080/#/database` | Trang Database (nút SSH) |

Đăng nhập: `admin` / `labadmin`.

Kiểm tra nhanh ba thứ, nếu sai thì sửa trước khi demo:

```sh
# 0. Lấy cookie đăng nhập (mọi lệnh dưới đều cần, vì API không mở công khai)
CK=$(curl -s -i -X POST http://192.168.23.10:8080/api/login \
      -H 'Content-Type: application/json' \
      -d '{"username":"admin","password":"labadmin"}' \
     | grep -i '^set-cookie' | sed 's/.*sess=\([^;]*\).*/sess=\1/')

# 1. poller còn sống và đúng nhịp 5 giây
curl -s -H "Cookie: $CK" http://192.168.23.10:8080/api/poll
#    -> {"age_s":6,"healthy":true,"interval_s":5,...}   age_s phải < 10

# 2. Leases và Monitor khớp nhau
python3 /root/sync_check.py      # in ra MISMATCHES: 0

# 3. container demo SSH còn chạy
docker ps --filter name=ssh-demo --format '{{.Status}}'
#    -> Up ... (healthy)
```

> Lưu ý: `/api/poll` **cần đăng nhập** — gọi trơn sẽ trả
> `{"detail":"auth required"}`. Đó là hành vi đúng, không phải lỗi.

Nếu `age_s` lớn hoặc poller chết:

```sh
doas rc-service lab-poller restart     # nếu chạy bằng OpenRC
# hoặc
cd /root/lab && doas docker compose restart poller
```

---

## 1. Mở đầu — bài toán (1 phút)

> "Đây là hệ thống giám sát cho lab mạng 4 router, 2 switch và 8 máy trạm.
> Toàn bộ hạ tầng được dựng lại bằng Ansible, nên nếu xoá máy chủ thì chỉ
> cần chạy lại playbook là có nguyên lab.
>
> Điểm khó không phải là vẽ được sơ đồ. Điểm khó là **hai trang phải nói
> cùng một sự thật**. Em sẽ bắt đầu bằng đúng chỗ từng bị sai."

---

## 2. Trang Monitor — cái gì đang sống (2 phút)

Mở **tab 1**.

Chỉ vào các ô đếm phía trên sơ đồ. Chúng có ba trạng thái, không phải hai:

> "Ở đây có ba trạng thái, không phải xanh/đỏ.
>
> **up** — thiết bị trả lời SNMP, em biết cả CPU lẫn RAM.
> **blind** — thiết bị trả lời ping nhưng không trả lời SNMP. Nó **đang
> sống**, em chỉ không đọc được thông số. Nếu em gọi nó là 'down' thì sai.
> **down** — không ping được, thật sự mất.
>
> Trạng thái thứ hai là lý do em không dùng hai màu. Một máy Ubuntu bật
> nhưng chưa cài snmpd sẽ bị báo đỏ nếu chỉ có hai màu — và báo sai."

Chỉ vào `controller`, rồi vào `r1`:

> "Cùng một lúc: `r1` hiện **up** vì SNMP trả lời. `controller` chỉ hiện
> **up** khi dịch vụ giám sát trên chính nó chạy — không có chuyện nó tự
> coi mình là trên cùng một thang đo với router."

Bấm vào một thiết bị đang **up** để mở panel thông số:

> "CPU, RAM, uptime — lấy từ SNMP mỗi 5 giây. Không có số liệu cũ nào
> được giữ lại: nếu lần đọc này lỗi thì giá trị biến mất, chứ không đứng
> im ở số cũ."

**Nếu giáo sư hỏi "sao biết 5 giây là thật?"** — mở terminal, chạy:

```sh
curl -s -H "Cookie: $CK" http://192.168.23.10:8080/api/poll
```

> "`age_s` là số giây kể từ lần quét xong gần nhất. Cứ refresh vài lần,
> nó luôn dưới 10. Trước đây con số này là 226 giây — vì 11 thiết bị chết
> bị quét tuần tự, mỗi cái chờ timeout 8 giây. Giờ quét song song 16 luồng
> nên vòng lặp giữ được 5 giây. Hiện tại đã tích luỹ hơn 147.000 mẫu."

---

## 3. Trang Leases — cùng sự thật, góc nhìn khác (2 phút)

Mở **tab 2**.

> "Trang này trả lời câu hỏi khác: **địa chỉ IP nào đang được dùng**.
> Nó đọc bảng ARP của từng router."

Chỉ vào dòng đếm trên đầu trang:

> "23 địa chỉ đã thấy, 19 đang online. Nhưng điều quan trọng hơn con số:
> nó **phải khớp** với trang Monitor."

Đặt hai tab cạnh nhau (hoặc dùng phím tắt chuyển qua lại):

> "Cùng một thiết bị, hai trang. Nếu Monitor nói `sim-camera` down thì
> Leases cũng phải nói địa chỉ `.157` offline. Nếu lệch, một trong hai
> trang đang nói dối — và người trực ca sẽ tin trang sai."

**Đây là chỗ để làm phép thử.** Mở terminal, tắt một container:

```sh
doas docker stop sim-camera
```

Đợi ~10 giây, refresh cả hai tab:

> "Monitor: `sim-camera` chuyển **down**. Leases: địa chỉ `.157` chuyển
> **offline**, và cột *Last seen* **đứng yên** — vì không có bằng chứng
> mới nào để cập nhật. Đó là điểm mấu chốt: `last_seen` chỉ nhích khi có
> thiết bị thật trả lời."

Bật lại:

```sh
doas docker start sim-camera
```

Đợi ~15 giây, refresh:

> "Cả hai tự về lại. Không cần bấm gì."

**Giải thích nguyên nhân gốc** (đây là phần ăn điểm):

> "Trước đây hai trang lệch nhau, và em tưởng là do ARP cũ. Sai.
>
> Container `sim-*` dùng macvlan. Trong Linux, một macvlan con là **anh em**
> với card mạng thật của máy chủ, không phải con của nó. Nên máy chủ không
> ping được chính các container của mình — khung tin đi ra `eth0`, đích là
> MAC của container, và không bao giờ quay lại. `docker exec` vào trong
> container thì ping được, nhưng từ ngoài thì không.
>
> Hệ quả: Monitor báo 11 thiết bị down — **Monitor đúng**. Leases vẫn thấy
> chúng trong ARP nên báo online — **Leases sai**, vì ARP chỉ có nghĩa
> 'chưa hết hạn', không có nghĩa 'đang sống'.
>
> Cách sửa: tạo thêm một card macvlan phụ trên máy chủ (`mvl0`, mode
> bridge) và thêm route tới dải `.144/28`. Giờ máy chủ nhìn thấy container
> của mình như một thiết bị khác trong cùng dải.
>
> Và Leases giờ **không tự quyết định** nữa: nó lấy kết quả ping của
> poller. Một nguồn sự thật, hai cách hiển thị."

Nếu muốn chứng minh bằng số:

```sh
python3 /root/sync_check.py
# -> leases rows: 23  devices: 16  compared: 12
# -> MISMATCHES: 0
```

> "Script này so từng địa chỉ một. 12 địa chỉ so được, 0 cái lệch.
> Nó trả mã thoát 1 nếu có lệch — nên dùng được trong cron hay CI, chứ
> không chỉ để in ra cho đẹp."

---

## 4. Trang Database — bấm nút là vào máy (2 phút)

Mở **tab 3**.

> "Đây là kho thiết bị. Ngoài thông tin, mỗi máy có một nút **Connect SSH**
> và mật khẩu riêng của máy đó."

Chỉ vào cột mật khẩu:

> "Mật khẩu được mã hoá AES-GCM trong SQLite, không nằm dạng chữ thường.
> Bấm con mắt để xem, có ghi log ai xem."

Bấm **Connect SSH** trên dòng `ssh-demo` (hoặc `sim-fileserver`):

> "Đây là terminal thật, không phải giả lập. Nó mở một phiên SSH thật qua
> WebSocket tới sshd trong container."

Gõ:

```
echo DEMO_MARKER_$((6*7))
```

Kết quả: `DEMO_MARKER_42`.

> "42 tính bằng shell trên máy đích. Nếu đây là ảnh chụp hay text giả thì
> không tính được phép nhân."

Gõ thêm:

```
id; uname -a
```

> "Đây là user và kernel thật của container đó."

**Nếu giáo sư hỏi "sao `r1` không vào được?"** — thử bấm `r1`:

> "Router cấu hình chỉ cho phép khoá công khai, không cho mật khẩu. Nên
> nút này trả về 502 kèm lý do. Đó là **sự thật về credential**, không
> phải terminal hỏng. Em cố ý để nó báo đúng như vậy thay vì giả vờ
> thành công."

---

## 5. Trang Compliance — cấu hình có bị lệch không (1 phút)

Chuyển sang `#/compliance`.

> "Phần này so cấu hình đang chạy với cấu hình mong muốn. Nó chạy trên cả
> router và switch."

Bấm chạy thu thập, chỉ vào kết quả:

> "Mỗi quy tắc là một dòng: khớp thì xanh, lệch thì đỏ kèm lý do. Đây là
> kiểm tra read-only — không sửa gì trên thiết bị."

---

## 6. Điện thoại — cùng một hệ thống, màn hình nhỏ (1 phút)

Đây là phần dễ gây ấn tượng nhất nếu giáo sư cầm điện thoại.

Mở `http://192.168.23.10:8080` **trên điện thoại** (cùng wifi lab).

> "Giao diện tự nhận biết thiết bị. Trên điện thoại, sidebar thành thanh
> ngang ở trên, các cột xếp thành một cột dọc, và **không chữ nào bị bẻ
> giữa từ**."

Nếu không có điện thoại, dùng DevTools: `F12` → chọn điện thoại → chọn
iPhone SE (375px) → refresh.

**Chỗ này từng lỗi, và cách sửa là phần đáng nói:**

> "Trước đây trên điện thoại, tiêu đề hiện mỗi dòng **một ký tự**. Nguyên
> nhân: sidebar được đặt `width: 100%`, nhưng khung cha vẫn xếp **ngang**.
> Trong flexbox, một phần tử rộng 100% nằm trong hàng ngang sẽ chiếm hết
> chỗ, đẩy phần nội dung còn lại đúng bằng phần padding của nó — **18
> pixel**. Em đo được: nội dung rộng 18px, cao 1361px.
>
> Cách sửa: cho khung cha xếp **dọc**. Nghe thì tầm thường, nhưng nó là
> lỗi chỉ hiện trên điện thoại, hoàn toàn vô hình trên máy tính — nên
> không thể phát hiện bằng cách 'mở lên xem thử'."

Nếu muốn chứng minh bằng số, chạy trên máy có Chrome và Node 22:

```sh
cd files/ops && node sweep_layout.js
# -> BAD ROWS: 0 of 30
```

> "Script này đo 6 trang × 5 độ rộng màn hình: 390, 768, 1024, 1280,
> 1476. Nó tìm ba thứ: tràn ngang, chữ bị ép thành cột dọc, và ô bị cắt.
> 30/30 sạch. Nó trả mã thoát bằng số dòng lỗi nên dùng được trong CI."

---

## 7. Kết — điều đáng nhớ (30 giây)

> "Ba điều em muốn giáo sư nhớ:
>
> **Một:** hạ tầng này dựng lại được. Xoá máy chủ, chạy playbook, có lại
> lab. Không có bước làm tay nào.
>
> **Hai:** hệ thống có ba trạng thái chứ không phải hai. 'Không đọc được'
> khác 'đã chết' — và gộp hai cái đó lại là báo động sai.
>
> **Ba:** mọi thứ em vừa nói đều đo được, không phải cảm nhận. 0/12 lệch
> giữa hai trang. 30/30 bố cục sạch. `age_s` dưới 10 giây. Nếu một con số
> đó sai, em muốn biết trước giáo sư."

---

## Phụ lục A — Nếu có sự cố giữa buổi demo

| Hiện tượng | Kiểm tra | Sửa |
|---|---|---|
| Trang trắng / không đăng nhập được | `curl -s http://192.168.23.10:8080/healthz` | `cd /root/lab && doas docker compose up -d` |
| Số liệu không nhích | `curl -s -H "Cookie: $CK" .../api/poll` xem `age_s` | `doas docker compose restart poller` |
| Monitor báo tất cả down | `doas python3 /root/lab-sim/simctl.py status` | `doas python3 /root/lab-sim/simctl.py shim` rồi `up` |
| Nút SSH báo 502 | `docker ps --filter name=ssh-demo` | `doas docker start ssh-demo` |
| Leases lệch Monitor | `python3 /root/sync_check.py` | xem mục 3 — thường là poller hoặc shim |

## Phụ lục B — Câu hỏi giáo sư hay hỏi

**"Sao không dùng Prometheus/Grafana?"**
> "Vì bài toán ở đây là một lab nhỏ, chạy được trên một máy. Prometheus
> cần thêm hai dịch vụ và một ngôn ngữ truy vấn nữa để học. Em chọn SQLite
> cộng một poller — ít bộ phận hơn thì ít chỗ hỏng hơn. Nếu lab lớn lên
> thì đổi được, vì dữ liệu đã nằm trong bảng có cấu trúc."

**"Làm sao biết là không bịa số?"**
> "Mỗi con số đều truy được về một lệnh. `age_s` từ `/api/poll`, `online`
> từ kết quả ping của poller, CPU từ SNMP. Và em có script kiểm tra chéo
> (`sync_check.py`, `sweep_layout.js`) để chứng minh hai nguồn khớp nhau —
> đó là thứ không thể giả được nếu chỉ chụp màn hình."

**"Phần nào khó nhất?"**
> "Không phải phần code. Phần khó là **tin vào trang nào** khi hai trang
> nói khác nhau. Em đã mất một buổi đi tìm ARP cũ, trong khi sự thật là
> Monitor đúng và Leases sai, vì máy chủ không ping được container của
> chính nó. Bài học: khi hai nguồn lệch nhau, việc đầu tiên là kiểm tra
> **nguồn nào thật sự đo được**, chứ không phải sửa cái nào trông sai hơn."
