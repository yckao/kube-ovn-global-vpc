#!/usr/bin/env python3
"""Generate the Traditional Chinese managed-VPC engineering briefing.

Only writes docs/managed-solution-diagrams. Render SVGs with the existing
export-presentation-diagrams.cjs script to obtain 4K PNGs and layout receipts.
The diagrams describe the managed v1alpha2 path, not the historical D/OVN-IC
experiments. Source references and presentation notes are emitted together.
"""
from html import escape
from pathlib import Path
import json

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "docs" / "managed-solution-diagrams"
OUT.mkdir(parents=True, exist_ok=True)
P = dict(bg="#F5F7FB", white="#FFFFFF", ink="#162640", muted="#53647C",
         line="#D6E0EC", wire="#74859D", blue="#2563EB", bluefill="#EDF3FF",
         teal="#087F83", tealfill="#E8F7F5", purple="#7752C6", purplefill="#F2EDFC",
         amber="#AD6800", amberfill="#FFF4DD", red="#B83D58", redfill="#FFF0F3")
SLIDES = []


class Drawing:
    def __init__(self, number, slug, title, subtitle, message, sources, notes):
        self.number, self.slug = number, slug
        self.entry = dict(number=number, slug=slug, title=title, subtitle=subtitle,
                          message=message, sources=list(dict.fromkeys(sources)), notes=notes)
        self.a = ['<svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080" viewBox="0 0 1920 1080" role="img" aria-labelledby="title desc">',
                  f'<title id="title">{escape(title)}</title><desc id="desc">{escape(subtitle)}</desc>',
                  '<defs><style>text{font-family:"PingFang TC","Heiti TC","Noto Sans CJK TC",Arial,sans-serif}</style>']
        for color in ("wire", "blue", "teal", "purple", "amber", "red"):
            self.a.append(f'<marker id="a-{color}" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M1 1 L9 5 L1 9 Z" fill="{P[color]}"/></marker>')
        self.a.append('</defs>')
        self.rect(0, 0, 1920, 1080, "bg", stroke=None, r=0)
        self.rect(64, 32, 7, 23, "teal", stroke=None, r=3)
        self.text(86, 51, "GLOBAL VPC  /  ENGINEERING BRIEFING", 18, "teal", 700)
        self.text(1856, 51, "MANAGED v1alpha2 · v0.1.0", 18, "muted", anchor="end")
        self.text(64, 114, title, 44, weight=700, mw=1792)
        self.text(64, 158, subtitle, 24, "muted", mw=1792)
        self.line("M64 185 H1856", "line", 1)
        self.rect(64, 934, 1792, 64, "tealfill", stroke=None, r=14)
        self.text(88, 974, message, 26, "teal", 600, mw=1744)
        self.line("M64 1017 H1856", "line", 1)
        self.text(64, 1051, "架構與操作圖解  ·  詳細操作與適用邊界請見同目錄講解", 18, "muted", mw=1500)
        self.text(1856, 1051, f"{number:02d} / 12", 19, "muted", 700, anchor="end")

    def rect(self, x, y, w, h, fill="white", stroke="line", r=16):
        self.a.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" fill="{P.get(fill, fill)}" stroke="{P.get(stroke, stroke) if stroke else "none"}" stroke-width="1.5"/>')

    def text(self, x, y, text, size=24, color="ink", weight=400, anchor="start", mw=None):
        self.a.append(f'<text xml:space="preserve" x="{x}" y="{y}" font-size="{size}" fill="{P[color]}" font-weight="{weight}" text-anchor="{anchor}"'+(f' data-max-width="{mw}"' if mw else '')+f'>{escape(str(text))}</text>')

    def lines(self, x, y, lines, size=24, color="muted", gap=36, mw=None):
        for i, line in enumerate(lines):
            self.text(x, y + i*gap, line, size, color, mw=mw)

    def line(self, d, color="wire", width=3, end=False, both=False, dash=False):
        self.a.append(f'<path d="{d}" fill="none" stroke="{P[color]}" stroke-width="{width}" stroke-linecap="round" stroke-linejoin="round"'+(' stroke-dasharray="8 7"' if dash else '')+(f' marker-end="url(#a-{color})"' if end or both else '')+(f' marker-start="url(#a-{color})"' if both else '')+'/>')

    def box(self, x, y, w, h, title, lines=(), color="blue", size=27, fill=None):
        self.rect(x, y, w, h, fill or color+"fill")
        self.rect(x, y+18, 5, h-36, color, stroke=None, r=2)
        self.text(x+24, y+42, title, size, color, 650, mw=w-48)
        self.lines(x+24, y+80, lines, 23, gap=34, mw=w-48)

    def pill(self, x, y, w, label, color="blue", size=20):
        self.rect(x, y, w, 38, color+"fill", stroke=None, r=19)
        self.text(x+w/2, y+26, label, size, color, 600, "middle", mw=w-20)

    def note(self, x, y, w, title, lines, color="muted"):
        self.text(x, y, title, 27, color, 650, mw=w)
        self.lines(x, y+39, lines, 23, gap=34, mw=w)

    def finish(self):
        self.entry.update(svg=self.slug+".svg", png=self.slug+".png")
        (OUT / self.entry["svg"]).write_text('\n'.join(self.a+['</svg>'])+'\n')
        SLIDES.append(self.entry)


def experience():
    s = Drawing(1, "01-platform-experience", "使用者描述網路，平台完成跨站連線",
                "接近 GCP 的操作模型：Project → Global VPC → 各 location 的 Subnet",
                "使用者不維護 Gateway IP、金鑰、ASN 或 peer list；這些資訊由平台產生與交換。",
                ["docs/managed-quickstart.md", "api/v1alpha2/types.go", "internal/platformcontroller"],
                ["先從使用者視角介紹：在 project namespace 建立一個 VPC，再為 dc-a、dc-b 建立 Subnet。VPC 名稱與預設 class 就足以建立全域意圖；子網只需 vpcRef、locationRef、cidr。",
                 "此處的 global 是路由與資源管理範圍；一個 Subnet 仍只屬於一個 Infra/DC，並非跨站延伸 L2，也不宣稱具備完整 GCP 產品能力。",
                 "單站只有本地子網時，建立原生 Kube-OVN Vpc/Subnet，不啟動跨站 Gateway；有遠端子網後才配置 Gateway 與跨站路由。",
                 "Compute 平台仍需實作 nativeSubnetName 查詢與 Pod/VM 接網介面。圖中的使用者體驗是已實作 API 的入口；租戶 UI 與 compute adapter 尚未實作。"])
    s.box(64, 222, 492, 624, "使用者輸入（節錄）", ["Project  ·  project-demo"], "blue")
    s.rect(88, 332, 444, 200, "white")
    s.lines(112, 372, ["kind: VPC", "metadata:", "  name: production", "spec: {}"], 25, "ink", 38, 394)
    s.rect(88, 552, 444, 228, "white")
    s.lines(112, 592, ["kind: Subnet", "spec:", "  vpcRef: production", "  locationRef: dc-a", "  cidr: 10.60.1.0/24"], 23, "ink", 36, 394)
    s.line("M556 505 H627", "blue", end=True)
    s.box(636, 302, 500, 390, "平台自動完成", ["驗證專案、位置與 CIDR", "建立 Kube-OVN 原生資源", "選擇多 Gateway、分配位址", "交換成員資訊、建立 peer", "設定路由、BFD 與狀態回報"], "teal")
    s.line("M1136 505 H1207", "teal", end=True)
    s.rect(1216, 222, 640, 624, "white")
    s.text(1244, 268, "VPC  ·  production", 29, "purple", 650)
    s.box(1244, 306, 584, 160, "DC-A / app-a", ["10.60.1.0/24", "本地 native Vpc / Subnet"], "purple")
    s.line("M1536 474 V568", "amber", 5, both=True)
    s.text(1568, 530, "跨站 L3 路由", 23, "amber")
    s.box(1244, 580, 584, 160, "DC-B / app-b", ["10.61.1.0/24", "本地 native Vpc / Subnet"], "purple")
    s.text(1244, 804, "工作負載透過 nativeSubnetName 接網", 23, "muted", mw=584)
    s.text(64, 894, "產品邊界：API 已實作；租戶 UI、Compute／KubeVirt 接網 adapter 仍待平台整合。", 24, "muted", mw=1792)
    s.finish()


def components():
    s = Drawing(2, "02-component-architecture", "集中管理意圖，各 site 自主執行",
                "Authority 不在封包路徑；每站維持獨立 Kubernetes API、OVN 資料庫與已接受的本地狀態",
                "Site Operator 不直接呼叫彼此；中央服務中斷時，既有本地狀態與轉送可持續。",
                ["cmd/platform-vpc-controller/main.go", "internal/localcontroller/sync.go", "config/managed/authority.yaml", "config/managed/site.yaml"],
                ["Authority 讀取 public VPC/Subnet，驗證與配置全域資源，為各 location 產生 NetworkBinding。管理叢集 API 同時保存意圖與交換所需資訊。",
                 "每站 Site Operator 從自己的 authority namespace 取回 Binding，保存到本地 API，再協調 Kube-OVN 原生資源與 Gateway runtime。虛線表示狀態回報，而非封包路徑。",
                 "Authority 與各站 operator 都是兩副本、Lease leader election；同一責任範圍只有 leader 執行。兩副本 Deployment 並不代表 management etcd 跨站災難恢復已完成。",
                 "各站的 operator 不建立直接 RPC mesh；但 Gateway runtime 仍然需要對端 Gateway 的 endpoints、路由與協定會話。失去中央 API 時既有 dataplane 可繼續，新跨站意圖與成員資訊更新會等待恢復。"])
    s.rect(400, 214, 1120, 226, "bluefill")
    s.text(428, 255, "Management cluster  ·  HA API 為部署前提", 29, "blue", 650)
    s.box(428, 282, 440, 130, "Public API / Authority", ["VPC + Subnet  ·  leader + standby"], "blue", 26, "white")
    s.line("M868 347 H925", "blue", end=True)
    s.box(936, 282, 556, 130, "每 location 的 NetworkBinding", ["desired snapshot  +  discovery status"], "blue", 26, "white")
    s.line("M550 440 V495 H466 V541", "blue", end=True)
    s.line("M1370 440 V495 H1454 V541", "blue", end=True)
    s.line("M530 541 V478 H620 V447", "teal", end=True, dash=True)
    s.line("M1518 541 V478 H1440 V447", "teal", end=True, dash=True)
    s.text(64, 478, "實線：意圖／協調", 21, "blue")
    s.text(64, 513, "虛線：discovery／狀態", 21, "teal")
    for x, dc in ((64, "DC-A"), (1056, "DC-B")):
        s.rect(x, 536, 800, 366, "white")
        s.text(x+24, 580, dc+"  ·  independent Infra cluster", 28, "teal", 650, mw=748)
        s.box(x+24, 601, 752, 96, "Site Operator  ·  leader + standby", [], "teal")
        s.text(x+48, 677, "Syncer → 本地 Binding → local reconciliation", 23, "muted", mw=700)
        native_x, gateway_x = ((x+24, x+411) if dc == "DC-A" else (x+411, x+24))
        s.line(f"M{native_x+182} 697 V733", "purple", end=True)
        s.line(f"M{gateway_x+182} 697 V733", "amber", end=True)
        s.box(native_x, 740, 365, 136, "Kube-OVN", ["native CR → NB/SB → OVS"], "purple", 27)
        s.box(gateway_x, 740, 365, 136, "多 Gateway / VPC", ["FRR + transport runtime"], "amber", 27)
    s.line("M840 840 H1070", "amber", 5, both=True)
    s.text(961, 750, "Gateway", 21, "amber", anchor="middle")
    s.text(961, 781, "跨站資料流", 21, "amber", anchor="middle")
    s.finish()


def installation():
    s = Drawing(3, "03-installation-flow", "安裝分成平台一次性設定與使用者建網",
                "管理端與每個 Infra 分開安裝；先準備 native extension 與基礎網路，再驗證實際封包",
                "部署成功 ≠ 網路可用；最後必須取得 native ACK、Gateway 就緒，以及雙向封包證據。",
                ["docs/managed-quickstart.md", "config/examples/managed", "config/managed", "integration/kube-ovn/README.md"],
                ["平台管理者先確認 HA management API、各 Infra API、至少兩個可選 Gateway node、跨站可路由的 endpoint、Linux transport 能力、UDP 放行與 MTU。IPAM/NetBox 的 parent pools 必須先保留，不能直接把範例位址當正式分配。",
                 "建置 controller 與 gateway image 並使用 immutable digest。跨站必須準備和既有 Kube-OVN controller source 相符的 destination-routes.v1 extension，以及相應 CRD schema；只有套 schema 不足以完成安裝。",
                 "管理端套 public/internal CRD、RBAC、platform.json 與 Authority。每站記錄 clusterUID、標記 Gateway node，安裝 internal Binding CRD、site.json、location-scoped access 與 Site Operator。完整可執行步驟在 managed-quickstart.md。",
                 "開發 bootstrap helper 提供約一小時 access token；正式平台應接可更新的身分。這個原型沒有自動 token renewal，過期會停止管理同步，並不撤回已接受的本地 dataplane。",
                 "使用者提交 network.yaml 後，等待 VPC Ready，查詢 Subnet.status.nativeSubnetName，依 quickstart 建立 smoke Pods 並跑雙向 ping/wget。此圖是安裝順序，不能取代版本核對與實際參數。"])
    steps = [
        (64, 245, "1  準備基礎設施", ["HA management API + 各 Infra", "每站至少 2 個 Gateway nodes", "IPAM pools / UDP / underlay MTU"], "blue"),
        (676, 245, "2  準備映像與 Native 擴充", ["Controller / Gateway image digest", "核對 Kube-OVN source version", "每站安裝 extension binary + schema"], "purple"),
        (1288, 245, "3  安裝管理服務", ["VPC / Subnet / Binding CRDs", "RBAC + platform.json", "Authority：2 replicas / 1 leader"], "blue"),
        (1288, 570, "4  安裝每站 Operator", ["clusterUID + node labels + pools", "location-scoped access + site.json", "Binding CRD + Site Operator"], "teal"),
        (676, 570, "5  使用者建立網路", ["提交 VPC + 各 location Subnet", "解析 nativeSubnetName 接網", "等待 native / Gateway 狀態"], "teal"),
        (64, 570, "6  封包與權責驗證", ["雙向 ICMP / HTTP / TCP", "確認來源位址與租戶隔離", "留存 status、route、packet 證據"], "amber"),
    ]
    for x, y, title, lines, color in steps:
        s.box(x, y, 568, 240, title, lines, color)
    s.line("M632 362 H666", "blue", end=True)
    s.line("M1244 362 H1278", "blue", end=True)
    s.line("M1572 485 V558", "blue", end=True)
    s.line("M1288 690 H1254", "teal", end=True)
    s.line("M676 690 H642", "teal", end=True)
    s.text(64, 881, "安裝輸入：platform.json / site.json  ·  租戶輸入：network.yaml  ·  完整指令：docs/managed-quickstart.md", 23, "muted", mw=1792)
    s.finish()


def discovery():
    s = Drawing(4, "04-automatic-discovery", "Gateway discovery 使用既有 Kubernetes API",
                "沒有手動 IP list，也不額外建立 Directory service；成員資訊透過 NetworkBinding 交換",
                "中央目錄負責分發更新；各 site 持久保存已接受的 snapshot，中央失聯不等於立即拆線。",
                ["internal/localcontroller/sync.go", "internal/localcontroller/reconciler.go", "internal/managedgateway/README.md", "internal/platformcontroller"],
                ["站點 operator 以 Gateway node selector 讀取 Nodes，驗證 UID 與 endpoint，配置本地位址與 transport 需要的識別。WireGuard 私鑰只存放本地 immutable Secret；回報的是 public key 與可供對端使用的 discovery 資訊。",
                 "本地 discovery/status 上傳該站在 management API 的 NetworkBinding。Authority 把已授權且參與同一個 VPC 的位置與成員組成 peer snapshot，分發給各站。這個邏輯目錄沿用 HA management API，沒有另一套目錄資料庫。",
                 "Syncer 保存本地 snapshot，驗證 source UID、revision 與 generation。單純 list omission 或 API 不通不會觸發刪除；撤除需要明確的 desired-state 刪除流程。",
                 "加入很多 site 不需要人工維護 gateway IP，但 Gateway session/tunnel 數量仍增加。對稱每站 G 個 gateway、S 個參與站點時，完整跨站 member-pair 組合約為 S(S−1)G²/2；S=50、G=2 時約 4,900 對。這是拓撲估算，不等於每種 backend 的實際 interface 數或已測容量。",
                 "只有少數 site 參與某 VPC 時，只交換該 VPC 的參與者。成員自動發現不代表硬體替換完全自動：node UID/endpoint 變更與 Gateway 數量變更仍被 fencing 保護，需管理者 recovery 流程。"])
    s.box(64, 265, 510, 310, "① Local discovery", ["Gateway-labelled Nodes", "UID + endpoint + allocations", "本地 key / identity receipts", "Site Operator 發布 status"], "teal")
    s.line("M574 406 H683", "teal", end=True, dash=True)
    s.box(696, 265, 528, 310, "② HA management API", ["各 location 的 Binding.status", "Authority 彙整 VPC participants", "產生 peer snapshot / revision", "同一 VPC 的授權成員可見"], "blue")
    s.line("M1224 406 H1333", "blue", end=True)
    s.box(1346, 265, 510, 310, "③ 各站保存與套用", ["Syncer → local NetworkBinding", "generation / UID / revision fence", "Gateway config + native routes", "回報相應 revision 的 ACK"], "teal")
    s.line("M1601 575 V615 H960 V585", "teal", end=True, dash=True)
    s.text(960, 654, "持續回報 discovery 與 runtime 狀態，收斂為下一個 desired snapshot", 24, "teal", anchor="middle", mw=1600)
    s.box(64, 712, 560, 170, "Sparse VPC", ["只加入 DC-A / DC-B / DC-C", "不向未參與 site 建立 tenant peers"], "purple", 26)
    s.box(652, 712, 584, 170, "Broad VPC", ["可描述所有已授權 locations", "成員 mesh 成本隨站點數成長"], "purple", 26)
    s.box(1264, 712, 592, 170, "中央 API 無法存取", ["保留本地 snapshot 與既有轉送", "新意圖／成員更新等待恢復"], "amber", 26)
    s.finish()


def integration():
    s = Drawing(5, "05-kube-ovn-integration", "Kube-OVN 維持原生網路設定的控制權",
                "Global VPC 透過 native CR 宣告需求；Kube-OVN extension 完成目的路由、ECMP、BFD 與回報",
                "不讓兩個 Controller 搶寫同一組 OVN 路由：Global VPC 管意圖，Kube-OVN 管 native OVN 設定。",
                ["integration/kube-ovn/README.md", "internal/localcontroller/reconciler.go", "internal/managedgateway/README.md"],
                ["從左到右說明寫入權責：Site Operator 將 accepted Binding 轉成 native Vpc/Subnet，並設定 Vpc.spec.bfdPort、spec.destinationRoutes。native Kube-OVN controller 才是這些受管理目的路由的 NB 設定寫入者。",
                 "Kube-OVN 把路由與 BFD 在受 ownership fence 保護的交易中更新到 NB；northd 產生 SB 狀態，ovn-controller 配置各 host OVS。這裡的『單一 writer』指對 native 路由設定的責任，不是說 northd 等元件不寫自己的資料庫。",
                 "native extension 回報 destination-routes.v1 capability 與精確 generation/hash ACK。Local Operator 必須確認 ACK，再把相應狀態送回 Authority，不能只看到物件存在就宣告 Ready。",
                 "上游 stock Kube-OVN 沒有此原型所需的完整 destinationRoutes contract。部署要同時提供 schema 與 source-matched controller binary。不同版本的 controller binary 與 schema 不能因名稱相近而視為可互換。",
                 "IPAM 分三層：平台管理 parent pool/位置授權；Global VPC 分配連線 infrastructure identities/addresses；原生 Kube-OVN 負責 Subnet 內 endpoint IPAM。此路徑不是 built-in OVN-IC 共享 transit switch 的架構。"])
    s.box(64, 265, 390, 284, "Local Site Operator", ["讀 local NetworkBinding", "建立 native Vpc / Subnet", "寫入 bfdPort", "寫入 destinationRoutes"], "teal", 26)
    s.line("M454 401 H503", "teal", end=True)
    s.box(516, 265, 422, 284, "Native Kube-OVN", ["source-matched extension", "檢查 ownership / routes", "交易式更新路由與 BFD", "回報 generation + hash"], "purple", 27)
    s.line("M938 401 H987", "purple", end=True)
    s.box(1000, 265, 340, 284, "OVN pipeline", ["NB：目的路由與 BFD", "northd", "SB：logical flows", "ovn-controller"], "purple", 27)
    s.line("M1340 401 H1389", "purple", end=True)
    s.box(1402, 265, 454, 284, "Host OVS dataplane", ["Endpoint → VPC router", "ECMP + BFD next-hops", "指向 local Gateways", "由 Gateways 封裝跨站轉送"], "amber", 26)
    s.line("M727 549 V606 H259 V558", "teal", end=True, dash=True)
    s.text(832, 615, "ACK：相應 generation / hash 已被 native controller 接受", 23, "teal", mw=1020)
    s.box(64, 700, 552, 179, "平台 IPAM / 管理者", ["保留 workload / transit / control pools", "設定 locations 與 project 授權"], "blue", 27)
    s.box(648, 700, 568, 179, "Global VPC allocator", ["networkID / ASN / VNI / member 位址", "保留 allocation receipts 與 anchors"], "teal", 27)
    s.box(1248, 700, 608, 179, "Kube-OVN native IPAM", ["原生 Subnet 與 endpoint IP 配置", "維持原有 OVN / CNI 協調流程"], "purple", 27)
    s.finish()


def traffic():
    s = Drawing(6, "06-packet-path", "跨站封包走 Gateway，控制服務不在路徑上",
                "每個 VPC、每個 site 部署多個 Gateway；底層只需路由 outer endpoint，不交換 tenant routes",
                "正常流量可使用多個 Gateway；ECMP 分流以 flow 為單位，單一連線不等於聚合所有 NIC 頻寬。",
                ["gateway/managed.py", "internal/managedgateway/README.md", "integration/kube-ovn/README.md", "docs/managed-validation.md"],
                ["由左往右跟著一個封包走：A 的 endpoint 先進入本地 Kube-OVN VPC logical router；遠端目的前綴對應多個 Gateway next-hop，透過本地 BFD 健康狀態決定可用路徑。",
                 "Gateway 解讀租戶路由並透過選定 transport 封裝，outer source/destination 是可由既有 L3 underlay 路由的 Gateway node endpoints。B 端 Gateway 解封裝後再送回 B 的原生 VPC 與目的 endpoint。IPv4 路由設計保留 workload source address，部署時仍須用封包核對。",
                 "圖中畫 2×2 個跨站成員組合，代表 G2 下可用的 member mesh。橘色資料路徑可雙向；不是說每個 packet 都複製到四條路，也不保證 forward/reverse 選到同一個 Gateway。",
                 "本地 OVN→Gateway BFD 與 Gateway 間 FRR/BFD 觀測不同故障範圍。Authority、Site Operator、API 不在正常封包轉送鏈上，但依然負責新增意圖與恢復後的狀態收斂。",
                 "不修改 ToR 指不要求增加 tenant VLAN/VNI、tenant route 或 EVPN/BGP peer；前提仍是 outer endpoints 可達、必要 UDP 可通與 MTU 足夠。NIC 線速加總不能直接當成有效吞吐量。"])
    for x, title, subnet in ((64, "DC-A", "10.60.1.0/24"), (1200, "DC-B", "10.61.1.0/24")):
        s.rect(x, 247, 656, 635, "white")
        s.text(x+24, 290, title, 30, "teal", 700)
        s.text(x+630, 290, "native VPC", 23, "purple", anchor="end")
        s.box(x+32, 325, 592, 112, "Endpoint / Subnet", [subnet], "purple", 26)
        s.line(f"M{x+328} 437 V485", "amber", 2.6, both=True)
        s.box(x+32, 485, 592, 130, "OVN VPC logical router", ["remote destination → ECMP + local BFD"], "purple", 26)
        gx = x+340 if title == "DC-A" else x+32
        branch = x+210 if title == "DC-A" else x+446
        edge = gx if title == "DC-A" else gx+284
        s.line(f"M{branch} 615 V827", "amber", 4)
        s.line(f"M{branch} 709 H{edge}", "amber", 4, both=True)
        s.line(f"M{branch} 827 H{edge}", "amber", 4, both=True)
        s.box(gx, 660, 284, 100, "Gateway 1", ["node 1 · FRR"], "amber", 25)
        s.box(gx, 778, 284, 100, "Gateway 2", ["node 2 · FRR"], "amber", 25)
    s.rect(768, 290, 384, 245, "bluefill")
    s.text(960, 335, "既有 L3 ToRs / WAN", 28, "blue", 650, "middle", mw=340)
    s.text(960, 390, "只路由 outer endpoints", 24, "muted", anchor="middle", mw=340)
    s.text(960, 434, "不加入 tenant BGP / EVPN", 22, "muted", anchor="middle", mw=340)
    s.text(960, 480, "Transport 封包穿越此網路", 23, "blue", anchor="middle", mw=340)
    # Each member attaches to the full cross-site member mesh; no control API hop.
    s.line("M688 709 H1232", "amber", 4, both=True)
    s.line("M688 827 H1232", "amber", 4, both=True)
    s.line("M768 709 L1152 827", "amber", 3)
    s.line("M768 827 L1152 709", "amber", 3)
    s.text(960, 607, "2 × 2 member paths", 24, "amber", 650, "middle")
    s.text(960, 647, "Transport + FRR / BFD", 23, "amber", anchor="middle")
    s.line("M960 535 V569", "blue", 2, dash=True)
    s.text(960, 880, "中央 API 不經手資料封包", 22, "muted", anchor="middle", mw=440)
    s.finish()


def transports():
    s = Drawing(7, "07-transport-options", "三種 Transport，共用同一個平台 API",
                "由管理者以 NetworkClass 提供選項；租戶維持相同 VPC / Subnet 操作模型",
                "EVPN 在軟體 Gateway 間運作；ToR 不必成為 VTEP、Route Reflector 或 tenant BGP peer。",
                ["config/examples/managed/platform.json", "docs/managed-quickstart.md", "gateway/managed.py", "gateway/evpn.py", "gateway/overlay.py"],
                ["WireGuard/BGP 提供加密 transport，使用管理者保留的 UDP port range；安全性更直觀，但 CPU、加密吞吐與 MTU 必須依實際硬體量測。",
                 "Geneve/BGP 使用 native Geneve UDP 6082，適合可信的私有 underlay；不提供 WireGuard 的傳輸加密。6081 留給既有 Kube-OVN OVS Geneve，避免占用相同 port。",
                 "VXLAN/EVPN 使用控制 overlay UDP 4788 與 VXLAN data UDP 4789，Gateway FRR 以 EVPN Type-5 搭配 tenant VRF/L3VNI。底層網路只處理 outer IP，無須讓 ToR 加入租戶 EVPN。這個選項控制/資料模型較多，排障也更複雜。",
                 "目前 gateway 控制會話跨站採 eBGP，站內 sibling 路徑有 iBGP；ASN 由平台配置，使用者不用填。此部署選擇不表示所有 Global VPC 設計必須採 eBGP。",
                 "三種 profile 都需要獨立的封包與故障驗證，不能從低負載功能測試推導硬體 capacity。現有 VPC 的 transport 不能原地切換，需建立新 VPC 規劃遷移。"])
    profiles = [
        (64, "WireGuard / BGP", "default", "blue", ["加密：是", "UDP：保留的 port range", "路由：FRR IPv4 unicast", "適用：需要傳輸加密", "代價：加密 CPU / MTU 預算"]),
        (676, "Geneve / BGP", "trusted-geneve", "teal", ["加密：否，需可信 underlay", "UDP：6082", "路由：FRR IPv4 unicast", "適用：私有軟體 overlay", "代價：來源控管 / MTU 驗證"]),
        (1288, "VXLAN / EVPN", "trusted-evpn", "purple", ["加密：否，需可信 underlay", "UDP：control 4788 / data 4789", "路由：EVPN Type-5 + VRF", "適用：偏好 EVPN route model", "代價：控制與排障步驟較多"]),
    ]
    for x, title, cls, color, lines in profiles:
        s.box(x, 250, 568, 480, title, [], color, 31)
        s.pill(x+24, 320, 520, "NetworkClass: "+cls, color, 21)
        s.lines(x+24, 405, lines, 25, "ink", 57, 520)
    s.rect(64, 768, 1792, 126, "white")
    s.text(88, 810, "共同條件", 27, "ink", 650)
    s.text(288, 810, "可達的 node endpoints  ·  多 Gateway  ·  FRR / BFD  ·  端到端 MTU 與隔離驗證", 24, "muted", mw=1528)
    s.text(88, 861, "Transport 在 VPC 建立後固定；切換需遷移。功能驗證不代表任一 profile 的線速保證。", 24, "muted", mw=1744)
    s.finish()


def isolation():
    s = Drawing(8, "08-tenant-isolation", "CIDR 可以重疊，租戶的路由識別不能混在一起",
                "Project / VPC identity → native VPC → Gateway namespace / VRF → transport context",
                "不同 VPC 可使用相同 CIDR；同一 VPC 內各站 Subnet 不能重疊。",
                ["internal/platformplan", "internal/managedgateway/README.md", "gateway/managed.py", "docs/managed-validation.md"],
                ["用 Red、Blue 兩個租戶說明：它們在各自 A 站與 B 站使用相同 CIDR，但 native VPC、Gateway runtime/network namespace、transport context 都是隔離的。傳送時不能只憑 destination IP 決定 tenant。",
                 "WireGuard 依租戶/成員的 key 與路由配置；Geneve 用對應 VNI 與 source delegation；EVPN 使用 tenant VRF/L3VNI 與路由匯入範圍。圖將 backend 差異抽象成 transport context，實際識別請查各 backend 原始碼。",
                 "同一 VPC 內的 prefixes 必須可唯一路由，因此跨位置 Subnet 不能重疊。Authority 負責 prefix admission 與全域 identity 配置，local registry 保存分配與不可變 anchor。",
                 "Tenant 只應有 public VPC/Subnet 編輯權。NetworkBinding、operator namespace、native CR 與 status 屬平台內部，不能把這些寫入權限開給租戶。",
                 "隔離驗證應包含 receiver-side capture，確認禁止的封包沒有到達。此控制器尚未提供完整租戶 Firewall policy/UI，也不能把有限測試視為安全認證。"])
    for y, tenant, color in ((254, "Red", "red"), (518, "Blue", "blue")):
        s.rect(64, y, 1792, 232, color+"fill")
        s.text(88, y+44, "VPC "+tenant, 30, color, 700)
        s.box(290, y+30, 408, 169, "DC-A native VPC", ["10.60.1.0/24", "獨立 native resource identity"], color, 26, "white")
        s.box(776, y+30, 368, 169, "Tenant Gateways", ["network namespace / VRF", "獨立 key / VNI context"], color, 26, "white")
        s.box(1222, y+30, 606, 169, "DC-B native VPC", ["10.61.1.0/24", "同 CIDR 不代表同一個 route domain"], color, 26, "white")
        s.line(f"M698 {y+115} H766", color, 4, both=True)
        s.line(f"M1144 {y+115} H1212", color, 4, both=True)
    s.pill(768, 473, 386, "Red 與 Blue 不匯入彼此路由", "purple", 21)
    s.box(64, 784, 864, 108, "控制面隔離", ["租戶只操作 public API；Binding / native resources 由平台管理"], "teal", 25)
    s.box(960, 784, 896, 108, "配置身分隔離", ["UID / allocation anchors 防止遺失配置後意外重用舊識別"], "teal", 25)
    s.finish()


def ha():
    s = Drawing(9, "09-ha-failure-behavior", "HA 保住可用路徑，也明確處理所有路徑失效",
                "控制面與資料面分開看；有存活路徑時收斂，沒有路徑時對遠端前綴 fail closed",
                "HA 不代表零掉包；須分別量測收斂、封包損失、恢復與租戶隔離。",
                ["integration/kube-ovn/README.md", "docs/managed-validation.md", "docs/managed-validation.md"],
                ["正常時 remote parent prefix 拆成兩個更精確的 child prefixes，各 child 對應多個 BFD-protected Gateway next-hops，parent 保持 discard。健康路徑以最長前綴優先；所有 child next-hop down 時落到 parent discard。每個目的前綴是 2×N+1 條 native static routes。",
                 "單一 Gateway full/WAN/OVN path 故障時，本地與 gateway FRR/BFD 讓流量朝存活路徑收斂。應分別量測 held TCP、新建 TCP、UDP 與更新期間的錯誤，不預設 zero-loss failover。",
                 "Authority/Site Operator/native controller 停止與 OVN DB、northd、host dataplane 故障是不同範圍。控制程序停止的驗證不能當作 site 斷電或實體 NIC 故障的證據。",
                 "所有 WAN 路徑中斷時遠端不可達，仍須核對本地與其他租戶是否正常。all-local-BFD-down 驗收必須確認遠端目的不會因 default/NAT route 而洩漏。",
                 "Management API HA/etcd quorum 是部署前提與獨立設計工作。Gateways 分布到不同實體 host 才能建立硬體故障域隔離；模擬的多節點拓樸不能取代實體故障域驗證。"])
    for x, title, state, color, lines in [
        (64, "正常：2 個 Gateway", "G1  ✓       G2  ✓", "teal", ["remote child prefixes → ECMP", "parent prefix → discard"]),
        (676, "單一成員故障", "G1  ×       G2  ✓", "amber", ["BFD / FRR 收斂至存活路徑", "可能有短暫掉包與 TCP 重傳"]),
        (1288, "本地 BFD next-hop 全失效", "G1  ×       G2  ×", "red", ["OVN parent prefix → discard", "不落入 default / NAT 出口"]),
    ]:
        s.box(x, 248, 568, 273, title, [], color, 28)
        s.text(x+284, 356, state, 34, color, 650, "middle", mw=500)
        s.lines(x+24, 433, lines, 24, "ink", 43, 520)
    s.line("M632 371 H666", "amber", end=True)
    s.line("M1244 371 H1278", "red", end=True)
    s.rect(64, 565, 1792, 322, "white")
    s.text(88, 611, "故障範圍", 25, "ink", 650)
    s.text(590, 611, "既有資料面", 25, "ink", 650)
    s.text(1134, 611, "新變更 / 適用邊界", 25, "ink", 650)
    rows = [("Authority / 管理端失聯", "本地已接受狀態持續轉送", "新意圖與 peer 更新等待恢復"),
            ("Site / native controller 停止", "依賴既有 OVN 狀態轉送", "其他 OVN 元件仍在線；新配置受阻"),
            ("所有 Gateway / WAN 不可用", "遠端不可達；本地流量保留", "恢復可用路徑後再收斂"),
            ("實體 NIC / host / site 失效", "須依實際部署單獨驗證", "需多實體故障域與後續驗證")]
    for i, row in enumerate(rows):
        y=666+57*i
        s.line(f"M88 {y-31} H1832", "line", 1)
        for x, value, mw in zip((88,590,1134), row, (464,502,698)):
            s.text(x,y,value,23,"muted",mw=mw)
    s.finish()


def lifecycle():
    s = Drawing(10, "10-network-lifecycle", "新增站點靠 Subnet，移除依序撤回依賴",
                "使用者操作宣告式資源；平台透過 ownership、ACK 與 finalizer 維持生命週期一致性",
                "不以 API 暫時看不到資源作為刪除訊號；也不靠刪除配置紀錄或強制清掉 finalizer 來復原。",
                ["docs/managed-quickstart.md", "internal/platformcontroller", "internal/localcontroller", "docs/managed-validation.md", "docs/managed-validation.md"],
                ["建立流程：使用者提交 VPC 與第一站 Subnet，本地 native 網路先成立；新增第二站 Subnet 使 VPC 有跨站需求，Authority 產生雙站 bindings，站點 discovery、native ACK 與 Gateway runtime 收斂後才回報就緒。每種 transport 都需要完整 cold join 驗收。",
                 "刪除流程先移除工作負載/endpoint，再刪 Subnet。平台在有使用者或 native 資源仍在使用時阻擋不安全撤除，並等候必要的原生清理及 peer withdrawal ACK。所有子網移除後才刪 VPC。",
                 "最後一個遠端前綴消失後，本地只剩 local-only VPC 時 Gateway/remote routes 可退役；本地 Vpc/Subnet 在仍有本地需求時保留。allocation receipts/anchors 不因一般 VPC 刪除自動釋放重用。",
                 "刪除後同名重建有新 UID；incarnation fence 防止舊 Binding/status 被新資源接納。更換 Node UID/endpoint、調整 gatewayReplicas 或遺失 registry/key 需要明確管理者 recovery；不能描述成全自動硬體替換。",
                 "驗收應區分 functional completion 與 strict zero-error；新增、刪除、重新加入均需記錄流量中斷，而非只確認最終 Ready。"])
    s.pill(64, 226, 240, "建立 / 加入 site", "teal", 23)
    create = [(64,"提交 VPC / Subnet",["project + location + CIDR"]),
              (520,"生成 Binding",["prefix admission + discovery"]),
              (976,"套用本地配置",["native ACK + Gateway runtime"]),
              (1432,"Ready + 封包驗證",["再交付 workload attachment"])]
    for x,title,lines in create:
        s.box(x, 292, 424, 148, title, lines, "teal", 26)
        if x<1432:s.line(f"M{x+424} 366 H{x+445}","teal",end=True)
    s.pill(64, 494, 240, "移除 / 退役", "purple", 23)
    delete = [(64,"移除 workloads",["解除 endpoint 使用關係"]),
              (520,"刪除 Subnet",["in-use guard / withdrawal"]),
              (976,"等待清理與 ACK",["native routes / peer runtime"]),
              (1432,"最後刪除 VPC",["所有子資源先完成退役"])]
    for x,title,lines in delete:
        s.box(x, 560, 424, 148, title, lines, "purple", 26)
        if x<1432:s.line(f"M{x+424} 634 H{x+445}","purple",end=True)
    s.box(64, 762, 864, 132, "一般操作保留的紀錄", ["allocation receipts / anchors  ·  防止配置身分意外重用"], "blue", 26)
    s.box(960, 762, 896, 132, "仍需管理者處理的變更", ["node UID / endpoint replacement  ·  Gateway 數量  ·  lost registry"], "amber", 26)
    s.finish()


def operations():
    s = Drawing(11, "11-operations-readiness", "操作從 Public API 開始，排障沿權責向下追",
                "先辨識卡在哪一層，再核對該層的 generation / identity / route；避免直接改動 OVN 底層狀態",
                "Deployment Ready 是程序健康；VPC Ready 是配置條件；雙向封包測試才證明實際通路。",
                ["docs/managed-quickstart.md", "internal/localcontroller/reconciler.go", "integration/kube-ovn/README.md", "docs/managed-validation.md"],
                ["日常先看 management cluster 的 public VPC/Subnet。取得 Ready 條件、observedGeneration、Subnet.status.nativeSubnetName，再在對應 Infra 查看 local Binding 與 native CR。不要把兩個 API context 混用。",
                 "NativeCapabilityRequired 表示缺少支援的 native contract；核對匹配的 extension binary、schema 與 capability。NativeRoutesPending 則追 desired/observed generation 與 hash；只看 Deployment ready 無法判定路由設定是否已接受。",
                 "GatewayPending 往選定 node、配置分配、runtime ConfigMap、Pod readiness、本地 BFD 與 retained peer BGP/BFD 追查。Gateway Pod Ready 不包含所有 end-to-end 路徑，因此最後仍須 packet test。",
                 "為避免命令混淆，圖中使用全名複數資源：vpcs.platform.globalvpc.io 是管理端 public API；Infra 的原生資源是 vpcs.kubeovn.io / subnets.kubeovn.io。完整 create/verify/delete 與 bootstrap 指令請用 managed-quickstart.md。",
                 "故障復原保留 allocation receipts、UID fences 與 finalizer 原則。不要透過刪 registry、重建 key 或強制移除 finalizer 讓條件表面變綠；遇到不一致要先保存 identity 與狀態證據，再走對應 recovery。"])
    tiers=[(64,"① Public intent","Management cluster",["VPC / Subnet conditions","observedGeneration / nativeSubnetName"],"blue"),
           (672,"② Accepted configuration","Local Infra cluster",["NetworkBinding → native Vpc","capability / generation / hash ACK"],"purple"),
           (1280,"③ Runtime + real traffic","Gateways / endpoints",["Pods / BFD / BGP / routes","雙向 ICMP / HTTP / TCP / tenant identity"],"amber")]
    for x,title,where,lines,color in tiers:
        s.box(x,248,576,238,title,[],color,26)
        s.text(x+24,331,where,23,color,600,mw=528)
        s.lines(x+24,391,lines,23,"ink",42,528)
        if x<1280:s.line(f"M{x+576} 365 H{x+596}",color,end=True)
    s.rect(64,528,1792,226,"white")
    s.text(88,571,"常見條件",26,"ink",650)
    s.text(678,571,"下一步",26,"ink",650)
    for i,(reason,action) in enumerate([
        ("NativeCapabilityRequired","核對 native extension binary、CRD schema 與 capability"),
        ("NativeRoutesPending","比對 destinationRoutes 的 desired / observed generation + hash"),
        ("GatewayPending","核對 node / allocation / runtime / local BFD，再查 peer 與封包")]):
        s.text(88,622+i*46,reason,24,"muted",mw=550)
        s.text(678,622+i*46,action,24,"muted",mw=1150)
    s.rect(64,786,1792,108,"bluefill",stroke=None)
    s.text(88,827,'kubectl --context "$AUTH_CTX" -n project-demo get vpcs.platform.globalvpc.io,subnets.platform.globalvpc.io',22,"blue",mw=1744)
    s.text(88,867,"部署與測試指令：docs/managed-quickstart.md  ·  配置可見性不取代封包驗證",24,"muted",mw=1744)
    s.finish()


def evidence():
    s = Drawing(12, "12-validation-and-boundaries", "v0.1.0：已有實作，正式採用仍需分層驗收",
                "Experimental release · 通用驗證方法與能力邊界；不代表特定環境、硬體容量或服務保證",
                "先完成原生整合與可重現的驗收，再依實際故障域、流量與站點規模決定採用範圍。",
                ["docs/managed-validation.md", "docs/managed-quickstart.md", "ROADMAP.md", "SECURITY.md"],
                ["公開版本包含 managed VPC/Subnet API、多 Gateway、三種 transport、原生 route contract 與對應的自動測試。這些是程式能力，不等於特定部署已經通過正式驗收。",
                 "Unit tests、真實 API integration、native/OVSDB tests 與 dataplane tests 各自支持不同結論。模擬 runtime ACK 或注入 BFD 狀態，不是硬體故障證據。",
                 "每個 transport 應核對雙向封包、來源位址、MTU、租戶隔離、成員故障與完整 join/leave/rejoin；分別記錄 functional acceptance 與 zero-error acceptance。",
                 "多 Gateway 的 flow distribution、頻寬聚合與單一連線速度要分開量測。真實 site 規模還受 session mesh、物件大小、CPU 與收斂成本影響。",
                 "正式採用前仍需平台接網 adapter、可更新身分、升級回滾、硬體故障域驗證、依賴掃描與發布供應鏈管理。"])
    s.box(64,246,568,640,"實作範圍",[],"teal",31)
    s.lines(88,339,["Managed VPC / Subnet API","各站持久化 snapshot","多 Gateway / ECMP / BFD","3 種 transport backends","Native Kube-OVN route contract","UID / ownership / allocation fence","Go / Python / API / native tests"],25,"ink",66,520)
    s.box(676,246,568,640,"驗收必須區分",[],"amber",31)
    s.lines(700,339,["Unit / API / real dataplane","Configured / Ready / reachable","功能完成 / 更新期間零錯誤","多 flow 分流 / 單一 flow 頻寬","固定 offered load / 最大吞吐量","模擬拓樸 / 實體故障域","API 規模 / 真實 site 規模"],25,"ink",66,520)
    s.box(1288,246,568,640,"採用前需完成",[],"purple",31)
    s.lines(1312,339,["匹配的 native binary + schema","平台接網與身分 renewal","完整生命週期與回滾驗證","NIC / host / site 故障驗證","容量與收斂預算量測","安全掃描與發布 provenance","支援範圍與維運責任定義"],25,"ink",66,520)
    s.finish()


def documents():
    manifest = dict(title="Global VPC — 工程圖解", edition="v0.1.0", architecture="managed v1alpha2",
                    audience="Platform and network engineers, including technical leadership",
                    canvas=dict(width=1920, height=1080, pngScale=2), slides=SLIDES)
    (OUT/"manifest.json").write_text(json.dumps(manifest,ensure_ascii=False,indent=2)+'\n')
    parts = ["# Global VPC — 工程圖解與講稿", "",
             "適用對象：平台／網路工程師與技術主管。這份圖集描述 v0.1.0 managed v1alpha2 實作、通用範例與驗收方法，不包含任何私人環境描述。", "",
             "建議完整講解約 35–45 分鐘：01–02 說明體驗與責任；03–05 說明部署、discovery 與 Kube-OVN；06–09 說明流量、選項、隔離與 HA；10–12 說明操作與成熟度。技術主管快速版可先看 01、02、06、09、12，約 12 分鐘。", "",
             "圖片內的 site / location 對應一個獨立 Infra/DC。Public VPC 是全域意圖；native Vpc 是每個 Infra 中的 Kube-OVN 物件。所有操作命令以 [managed quick start](../managed-quickstart.md) 為準；圖中名稱、位址與拓樸均為通用範例。", ""]
    for e in SLIDES:
        parts += [f'## {e["number"]:02d} · {e["title"]}', "", f'![{e["title"]}]({e["png"]})', "",
                  f'**一句話：** {e["message"]}', "", "**講解順序：**", ""]
        parts += [f'{i}. {n}' for i,n in enumerate(e["notes"],1)]
        parts += ["", "**來源：** " + " · ".join(f'[{p}](../../{p})' for p in e["sources"]), "",
                  f'[4K PNG]({e["png"]}) · [可編輯 SVG]({e["svg"]})', ""]
    (OUT/"speaker-notes.zh-TW.md").write_text('\n'.join(parts))
    readme = '''# Managed Global VPC engineering diagrams

A Traditional Chinese briefing for platform/network engineers and technical
leadership. Technical names remain in English. This package describes the
managed `platform.globalvpc.io/v1alpha2` implementation and generic validation guidance,
not the historical D/OVN-IC architecture or a production qualification.

- Open [index.html](index.html) for the offline gallery, per-image notes, and
  keyboard-operated presentation mode. Use arrow keys to navigate and Escape
  to exit the presentation. No CDN, web service or build step is needed to view it.
- Read [speaker notes](speaker-notes.zh-TW.md) for the 35–45 minute walkthrough.
  The leadership shortcut is images 01, 02, 06, 09 and 12.
- Each image is available directly as a **3840 × 2160 PNG** and an editable
  **1920 × 1080 SVG**. Drop the PNG files into any 16:9 presentation.
- [manifest.json](manifest.json) maps each figure to its implementation and
  evidence sources. The gallery contains the same notes and source links.
- [overview.png](overview.png) is a contact sheet, not the readable full-size edition.
  Its [HTML source](overview.html) can be captured at a 1800-pixel viewport width.

## Reproduction

From the repository root:

```sh
python3 scripts/render-managed-solution-diagrams.py
node scripts/export-presentation-diagrams.cjs /path/to/playwright /path/to/chromium docs/managed-solution-diagrams
```

The SVG generator also writes the gallery, manifest and speaker notes. The
Chromium exporter checks text bounds and text overlap and stores its report in
`artifacts/managed-solution-diagrams/render-qa.json`. Fonts use PingFang TC /
Heiti TC / Noto Sans CJK TC; rerendering on another system may need a CJK font.
Delivered PNGs have already been rendered and visually inspected.

## Scope and evidence

The canonical [quick start](../managed-quickstart.md),
[validation guidance](../managed-validation.md), and
[native integration contract](../../integration/kube-ovn/README.md) take precedence
above an abbreviated figure. All names, addresses and topology illustrations
are synthetic; no deployment inventories or private run evidence are included.

Version 0.1.0 is experimental and requires a source-matched Kube-OVN extension,
platform identity renewal and compute attachment integration. It does not claim
cloud product parity, stock-native support, zero-loss failover, physical HA,
a particular throughput or a supported production site count.
'''
    (OUT/"README.md").write_text(readme)
    cards=[]
    for e in SLIDES:
        links=" · ".join(f'<a href="../../{escape(p)}">{escape(p)}</a>' for p in e['sources'])
        notes=''.join('<li>'+escape(n)+'</li>' for n in e['notes'])
        cards.append(f'''<article id="slide-{e['number']}">
<div class="card-head"><span class="number">{e['number']:02d}</span><h2>{escape(e['title'])}</h2></div>
<button class="image-button" data-open="{e['number']-1}" aria-label="放大第 {e['number']} 張圖"><img src="{e['png']}" alt="{escape(e['title'])}" loading="lazy" width="1920" height="1080"></button>
<div class="card-body"><p class="key">{escape(e['message'])}</p><div class="actions"><a href="{e['png']}" download>下載 4K PNG</a><a href="{e['svg']}" download>下載 SVG</a><button data-open="{e['number']-1}">放大／簡報</button></div>
<details><summary>講解與技術細節 · {len(e['notes'])} 個重點</summary><ol>{notes}</ol><p class="sources">依據：{links}</p></details></div></article>''')
    data=json.dumps(SLIDES,ensure_ascii=False).replace('</','<\\/')
    html='''<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Global VPC · 工程圖解</title>
<style>
:root{--ink:#162640;--muted:#53647c;--teal:#087f83;--bg:#f5f7fb}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font-family:"PingFang TC","Heiti TC",system-ui,sans-serif}header,main,footer{max-width:1440px;margin:auto;padding:36px 40px}header{padding-top:64px}h1{font-size:clamp(32px,4vw,54px);margin:12px 0 18px;letter-spacing:-1px}header p{font-size:19px;line-height:1.8;max-width:1080px}.eyebrow{color:var(--teal);font-size:14px;font-weight:750;letter-spacing:2px}a{color:#245dcc;text-underline-offset:4px}button,a{touch-action:manipulation}button{font:inherit;cursor:pointer}.toolbar,.actions{display:flex;gap:14px;align-items:center;flex-wrap:wrap}.toolbar button,.actions button,.actions a{padding:10px 16px;border-radius:9px;border:1px solid #cdd9e7;background:white;color:var(--ink);font-size:15px;text-decoration:none}.toolbar button.primary{background:var(--teal);color:white;border-color:var(--teal)}.scope{border-left:4px solid #d18a29;padding:12px 20px;background:#fff4dd;border-radius:0 10px 10px 0;font-size:16px;line-height:1.8}.toc{display:grid;grid-template-columns:repeat(3,1fr);gap:12px;margin:30px 0}.toc a{padding:13px;background:white;border:1px solid #d6e0ec;border-radius:10px;text-decoration:none;line-height:1.5;font-size:15px}.toc b{color:var(--teal);margin-right:8px}article{background:white;border:1px solid #d6e0ec;border-radius:18px;overflow:hidden;margin-bottom:38px;scroll-margin-top:20px}.card-head{display:flex;align-items:center;gap:18px;padding:22px 28px}.number{background:#e8f7f5;color:var(--teal);font-size:20px;font-weight:700;padding:9px 12px;border-radius:10px}h2{font-size:24px;margin:0;line-height:1.5}.image-button{border:0;padding:0;width:100%;display:block;background:var(--bg)}img{display:block;width:100%;height:auto}.card-body{padding:10px 28px 26px}.key{font-size:19px;font-weight:600;line-height:1.7}.actions{margin-bottom:22px}details{border-top:1px solid #e5ebf1;padding-top:18px}summary{cursor:pointer;font-weight:600;font-size:17px}ol{font-size:18px;line-height:1.9;padding-left:25px}li{padding:5px 0 9px}.sources{font-size:14px;line-height:1.8;overflow-wrap:anywhere}.sources a{margin-right:7px}footer{color:var(--muted);line-height:1.8;font-size:15px;padding-bottom:80px}dialog{max-width:none;max-height:none;width:100vw;height:100dvh;margin:0;border:0;padding:0;background:#101b2d;color:white}dialog::backdrop{background:#101b2d}.viewer{height:100%;display:flex;flex-direction:column}.viewer-top,.viewer-bottom{display:flex;align-items:center;justify-content:space-between;gap:14px;padding:12px 22px;background:#15243b;min-height:58px}.viewer button{border:1px solid #52617b;border-radius:7px;background:transparent;color:white;padding:8px 13px}.viewer .stage{flex:1;min-height:0;display:flex;align-items:center;justify-content:center}.viewer img{max-width:100%;max-height:100%;width:auto;height:auto;object-fit:contain}.viewer .counter{font-variant-numeric:tabular-nums}.viewer-bottom span{font-size:15px;color:#cbd5e6}.hint{font-size:14px;color:var(--muted)}@media(max-width:800px){header,main,footer{padding:24px 18px}.toc{grid-template-columns:1fr}.card-head{padding:18px}h2{font-size:20px}.card-body{padding:5px 18px 24px}.viewer-bottom span{display:none}.key{font-size:17px}}@media print{@page{size:landscape;margin:0}header,.card-head,.card-body,footer{display:none}main{max-width:none;padding:0}article{border:0;border-radius:0;break-after:page;margin:0;height:100vh;display:flex;align-items:center}article img{width:100%}}
</style></head><body><header><div class="eyebrow">MANAGED v1alpha2 · ENGINEERING BRIEFING · v0.1.0</div><h1>Global VPC 工程圖解</h1><p>把跨站網路變成平台資源：以 GCP 式 VPC / Subnet 體驗為入口，整合 Kube-OVN 原生生命週期。12 張 4K 圖片，涵蓋安裝、元件、Discovery、封包路徑、Transport、HA 與操作。</p><div class="scope">對象：平台／網路工程師與技術主管。v0.1.0 為 experimental。所有圖例採通用範例；接網、身分整合、容量與實體 HA 需依部署條件完成驗收。</div><p class="hint">完整講解約 35–45 分鐘。主管快速版：01 → 02 → 06 → 09 → 12，約 12 分鐘。</p><div class="toolbar"><button class="primary" data-open="0">開始簡報</button><a href="speaker-notes.zh-TW.md">逐圖講稿</a><a href="../managed-quickstart.md">安裝 Quick start</a><a href="../managed-validation.md">驗證方法與邊界</a><a href="overview.png">總覽圖片</a></div><nav class="toc" aria-label="圖集目錄">TOC</nav></header><main>CARDS</main><footer>PNG：3840 × 2160 · SVG：1920 × 1080，可編輯向量文字。圖片及講稿使用相同來源；點開每張圖的技術細節可查看原始碼／文件依據。所有資產可離線開啟。</footer>
<dialog id="presentation" aria-label="圖解簡報"><div class="viewer"><div class="viewer-top"><span id="viewer-title"></span><button id="close">關閉 Esc</button></div><div class="stage"><img id="active-slide" alt=""></div><div class="viewer-bottom"><button id="previous">← 上一張</button><span>使用 ← → 切換；Home / End 到首尾；Esc 返回圖集</span><b class="counter" id="counter"></b><button id="next">下一張 →</button></div></div></dialog>
<script>const slides=SLIDEDATA;const dialog=document.getElementById('presentation');let current=0;let prior=null;function render(){const s=slides[current];document.getElementById('active-slide').src=s.png;document.getElementById('active-slide').alt=s.title;document.getElementById('viewer-title').textContent=s.title;document.getElementById('counter').textContent=`${current+1} / ${slides.length}`;document.getElementById('previous').disabled=current===0;document.getElementById('next').disabled=current===slides.length-1;}function show(n){current=n;prior=document.activeElement;render();dialog.showModal();document.getElementById('close').focus();}function move(d){current=Math.max(0,Math.min(slides.length-1,current+d));render();}document.querySelectorAll('[data-open]').forEach(b=>b.addEventListener('click',()=>show(Number(b.dataset.open))));document.getElementById('close').addEventListener('click',()=>dialog.close());dialog.addEventListener('close',()=>prior?.focus());document.getElementById('previous').addEventListener('click',()=>move(-1));document.getElementById('next').addEventListener('click',()=>move(1));document.addEventListener('keydown',e=>{if(!dialog.open)return;if(['ArrowRight','ArrowLeft','Home','End'].includes(e.key)){e.preventDefault();if(e.key==='Home'){current=0;render();}else if(e.key==='End'){current=slides.length-1;render();}else move(e.key==='ArrowRight'?1:-1);}});</script></body></html>'''
    toc=''.join(f'<a href="#slide-{e["number"]}"><b>{e["number"]:02d}</b>{escape(e["title"])}</a>' for e in SLIDES)
    (OUT/"index.html").write_text(html.replace('TOC',toc).replace('CARDS','\n'.join(cards)).replace('SLIDEDATA',data))
    thumbnails=''.join(f'<a href="{e["png"]}"><img src="{e["png"]}" alt="{escape(e["title"])}" width="1920" height="1080"></a>' for e in SLIDES)
    overview='''<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Global VPC · 圖集總覽</title><style>*{box-sizing:border-box}body{margin:0;padding:28px;background:#e9eff7;color:#162640;font-family:"PingFang TC","Heiti TC",system-ui,sans-serif}h1{font-size:32px;margin:0 0 8px}p{font-size:18px;color:#53647c;margin:0 0 22px}.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:18px}a{display:block;border:1px solid #d6e0ec;background:white;border-radius:8px;overflow:hidden}img{display:block;width:100%;height:auto}@media(max-width:700px){.grid{grid-template-columns:1fr}}</style></head><body><h1>Global VPC · 12 張工程圖解</h1><p>安裝與元件 → Discovery / Kube-OVN → 封包、Transport、隔離、HA → 操作與驗證邊界</p><div class="grid">THUMBNAILS</div></body></html>'''
    (OUT/"overview.html").write_text(overview.replace('THUMBNAILS',thumbnails))


if __name__ == "__main__":
    for draw in (experience, components, installation, discovery, integration, traffic,
                 transports, isolation, ha, lifecycle, operations, evidence):
        draw()
    documents()
    print(f"Generated {len(SLIDES)} SVGs, gallery, manifest and speaker notes in {OUT}")
