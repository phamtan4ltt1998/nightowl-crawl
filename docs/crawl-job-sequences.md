# Crawl Job Sequences — Tối ưu 3-Tier

**Liên quan**: [incremental-crawl-strategy.md](./incremental-crawl-strategy.md)  
**Date**: 2026-05-12

---

## Tổng quan scheduler

```mermaid
timeline
    title Timeline chạy trong 1 ngày
    section Mỗi giờ
        T+0h  : Tier 1 - Listing Monitor
        T+1h  : Tier 1 - Listing Monitor
        T+2h  : Tier 1 - Listing Monitor
    section Mỗi 6 giờ
        T+0h  : Tier 2 - Active Updater (hot)
        T+6h  : Tier 2 - Active Updater (hot+warm)
        T+12h : Tier 2 - Active Updater (hot)
        T+18h : Tier 2 - Active Updater (hot)
    section Mỗi 24 giờ
        T+0h  : Tier 2 - Warm stories
    section Monthly
        1st   : Tier 3 - Completed Audit
```

---

## Sequence 1 — Tier 1: Listing Monitor

> **Mục tiêu**: phát hiện truyện URL mới trên listing page, KHÔNG re-crawl existing.  
> **Chi phí**: cố định, không phụ thuộc số truyện trong DB.

```mermaid
sequenceDiagram
    participant Sched as Scheduler
    participant LM as ListingMonitor
    participant HTTP as Source HTTP
    participant DB as MySQL

    Sched->>LM: trigger (mỗi 1h)
    
    loop Mỗi enabled source
        LM->>HTTP: GET /listing-page?page=1
        HTTP-->>LM: HTML (danh sách truyện)
        LM->>LM: extract story URLs
        
        alt Có pagination
            LM->>HTTP: GET /listing-page?page=2..N
            HTTP-->>LM: HTML
            LM->>LM: collect all URLs
        end
        
        LM->>LM: slugs = StorySlugFromURL(urls)
        LM->>DB: SELECT slug FROM books WHERE slug IN (slugs)
        DB-->>LM: existing_slugs
        
        LM->>LM: new_slugs = all_slugs - existing_slugs
        
        alt new_slugs không rỗng
            loop Mỗi new story (concurrency=2)
                LM->>HTTP: GET story-page (meta)
                HTTP-->>LM: HTML
                LM->>LM: parse meta (title, author, status...)
                
                LM->>HTTP: GET chapter-list
                HTTP-->>LM: chapter URLs
                
                loop Mỗi chapter (workers=4)
                    LM->>HTTP: GET chapter-content
                    HTTP-->>LM: HTML
                    LM->>LM: parse → write .md to disk
                end
                
                LM->>DB: INSERT book + chapters (upsert)
                LM->>DB: UPDATE last_checked_at = NOW()
                DB-->>LM: OK
            end
        else Không có truyện mới
            LM->>LM: log "no new stories" → exit
            Note over LM: Chỉ tốn N_listing_pages requests, không vào DB write
        end
    end
    
    LM-->>Sched: done (elapsed, new_count)
```

**Số request điển hình** (không có truyện mới):
```
1 source × 10 listing pages = 10 requests
12 sources × 10 pages = 120 requests/giờ  ← cố định
```

---

## Sequence 2 — Tier 2: Active Updater (per run)

> **Mục tiêu**: check truyện `ongoing` có chương mới không, chỉ fetch nếu có.  
> **Key optimization**: "probe before fetch" — so sánh chapter count trước, tránh fetch nội dung thừa.  
> **⚠️ Truyện `full`/`hoàn thành` bị loại ngay tại bước query — không bao giờ vào vòng lặp.**

```mermaid
sequenceDiagram
    participant Sched as Scheduler
    participant AU as ActiveUpdater
    participant DB as MySQL
    participant Story as StoryProber
    participant HTTP as Source HTTP
    participant Disk as Disk

    Sched->>AU: trigger (mỗi 6h)
    
    AU->>DB: SELECT slug, source_url, chapter_count, status, last_checked_at<br/>FROM books<br/>WHERE status NOT IN ('full','hoàn thành','completed')<br/>AND last_checked_at < NOW() - INTERVAL threshold<br/>ORDER BY last_checked_at ASC LIMIT 200
    DB-->>AU: candidates[] — CHỈ truyện ongoing/đang ra
    
    Note over AU,DB: ══ FILTER GATE: truyện full/hoàn thành KHÔNG BAO GIỜ xuất hiện ở đây ══
    
    AU->>AU: prioritize: hot > warm > cold
    Note over AU: hot  = last new chapter ≤ 7 ngày → check interval 6h<br/>warm = 7–30 ngày → 24h<br/>cold = >30 ngày ongoing → 72h
    
    loop Mỗi candidate (concurrency=2)
        AU->>Story: Probe(source_url)
        
        Story->>HTTP: GET chapter-list page 1 only
        HTTP-->>Story: HTML
        Story->>Story: parse source_chapter_count
        Story-->>AU: {source_count, source_status}
        
        alt source_status == full/hoàn thành (mới detect)
            Note over AU: Source vừa đánh dấu truyện hoàn thành!
            AU->>DB: UPDATE books SET status='hoàn thành',<br/>last_checked_at=NOW() WHERE slug=?
            Note over DB: ★ Từ vòng sau: truyện này bị loại khỏi query<br/>→ không bao giờ check lại (trừ Tier 3 audit)
        else source_count <= db_chapter_count (ongoing, không có chương mới)
            AU->>DB: UPDATE last_checked_at = NOW() WHERE slug = ?
            Note over AU: Chỉ touch timestamp, không fetch gì thêm
        else source_count > db_chapter_count (ongoing, có chương mới)
            AU->>HTTP: GET story meta
            HTTP-->>AU: HTML
            AU->>AU: parse meta (title, status, cover...)
            
            AU->>AU: existing = existingChapterNumbers(disk)
            
            loop Chỉ chương MỚI (workers=4)
                AU->>HTTP: GET chapter-content
                HTTP-->>AU: HTML
                AU->>AU: parse → markdown
                AU->>Disk: write .md file
            end
            
            AU->>DB: UpsertStoryFromDir → UPDATE chapter_count, status
            AU->>DB: UPDATE last_checked_at=NOW(), source_chapter_count=?
            DB-->>AU: {new_chapters: N}
            
            alt meta.status == full/hoàn thành (vừa hoàn thành sau chapter này)
                AU->>DB: UPDATE books SET status='hoàn thành' WHERE slug=?
                Note over DB: ★ Truyện exit khỏi active pool — không check lại
            end
        end
    end
    
    AU-->>Sched: done {checked: 200, updated: 12, graduated_to_complete: 3, new_chapters: 47}
```

---

## Lifecycle truyện "full" — Tại sao không check lại

```mermaid
stateDiagram-v2
    [*] --> New: Tier 1 phát hiện URL mới
    New --> Ongoing: crawl xong, status=đang ra
    New --> Completed: crawl xong, status=full/hoàn thành
    Ongoing --> Ongoing: Tier 2 check → có chương mới
    Ongoing --> Completed: Tier 2 detect source_status=hoàn thành\n★ EXIT active pool
    Completed --> [*]: không check nữa
    Completed --> Ongoing: Tier 3 audit phát hiện tiếp tục ra\n(hiatus ended)

    note right of Completed
        Tier 2 query WHERE status NOT IN ('full','hoàn thành')
        → truyện này KHÔNG BAO GIỜ vào candidates[]
        → 0 request/ngày cho 60% kho truyện
    end note
```

**Hai con đường truyện vào trạng thái `Completed`**:

| Con đường | Trigger | Ai xử lý |
|-----------|---------|----------|
| Source đánh dấu "full" ngay khi crawl lần đầu | `meta.status == full` lúc INSERT | Tier 1 Listing Monitor |
| Source đánh dấu "full" sau N chương cuối | `source_status == full` trong probe | Tier 2 Active Updater |

**SQL filter gate** — hàng rào bắt buộc trong `GetActiveCandidates`:

```sql
SELECT slug, source_url, chapter_count, last_checked_at
FROM books
WHERE status NOT IN ('full', 'hoàn thành', 'completed', 'đã hoàn thành')
  AND last_checked_at < DATE_SUB(NOW(), INTERVAL ? SECOND)
ORDER BY last_checked_at ASC
LIMIT ?;
```

> Index `idx_check_priority (status, last_checked_at)` đảm bảo query này O(ongoing_count) chứ không phải O(total_books).

---

## Sequence 3 — Probe optimization chi tiết

> Flow trong `StoryProber.Probe()` — bước quyết định có crawl hay không.

```mermaid
sequenceDiagram
    participant AU as ActiveUpdater
    participant HTTP as Source HTTP
    participant DB as MySQL

    AU->>HTTP: GET {source_url} (chapter list, page 1 only)
    HTTP-->>AU: HTML

    AU->>AU: count = parse chapter count từ HTML
    Note over AU: Ưu tiên: số trang hiển thị rõ ("1500 chương")<br/>Fallback: đếm link chapter trên page 1 × số trang

    AU->>DB: SELECT chapter_count FROM books WHERE slug = ?
    DB-->>AU: local_count

    alt count <= local_count
        AU-->>AU: SKIP (no new chapters)
        Note over AU: 1 request duy nhất, không fetch nội dung
    else count > local_count
        AU-->>AU: FETCH (source_count - local_count chapters needed)
        Note over AU: Fetch chính xác số chương còn thiếu
    end
```

**So sánh số request**:

| Scenario | Cũ (recrawl_existing) | Mới (probe first) |
|----------|----------------------|-------------------|
| Truyện không có chương mới | meta(1) + chap-list(5+) + content(0) = **6+ req** | probe(1) = **1 req** |
| Truyện có 5 chương mới | meta(1) + chap-list(5) + content(5) = **11 req** | probe(1) + meta(1) + content(5) = **7 req** |

---

## Sequence 4 — Tier 3: Monthly Audit

> Spot-check truyện "completed" để phát hiện tiếp tục ra sau hiatus.

```mermaid
sequenceDiagram
    participant Sched as Scheduler
    participant Audit as AuditJob
    participant DB as MySQL
    participant HTTP as Source HTTP

    Sched->>Audit: trigger (1st của tháng, 03:00)
    
    Audit->>DB: SELECT slug, source_url, chapter_count<br/>FROM books<br/>WHERE status IN ('full','hoàn thành')<br/>ORDER BY RAND() LIMIT 50
    DB-->>Audit: 50 completed stories (2% sample)
    
    loop Mỗi story (concurrency=1, chậm để tránh block)
        Audit->>HTTP: GET chapter-list (probe only)
        HTTP-->>Audit: HTML
        Audit->>Audit: source_count = parse count
        
        alt source_count > db_chapter_count
            Note over Audit: Truyện tiếp tục ra sau khi đánh dấu completed!
            Audit->>DB: UPDATE books SET status='đang ra',<br/>last_checked_at=NULL WHERE slug=?
            Note over DB: last_checked_at=NULL → Tier 2 sẽ pick up ngay lần chạy tiếp
            Audit->>Audit: log WARN "slug reactivated: +N chapters"
        else OK
            Audit->>DB: UPDATE last_checked_at = NOW() WHERE slug=?
        end
    end
    
    Audit-->>Sched: done {audited: 50, reactivated: 2}
```

---

## Sequence 5 — Tương tác 3 tiers trong 1 ngày (collision handling)

```mermaid
sequenceDiagram
    participant Sched as Scheduler
    participant T1 as Tier1 (Listing)
    participant T2 as Tier2 (Active)
    participant DB as MySQL

    Note over Sched: T+0h: cả 2 jobs trigger cùng lúc
    
    par Chạy song song
        Sched->>T1: start listing monitor
    and
        Sched->>T2: start active updater
    end
    
    Note over T1,T2: Không lock nhau — cùng đọc DB, write khác slug

    T1->>DB: SELECT existing slugs (read)
    T2->>DB: GetActiveCandidates (read)
    
    T1->>DB: INSERT new story "truyen-abc" (write)
    T2->>DB: UPDATE "truyen-xyz" chapter_count (write)
    
    Note over DB: Khác slug → không conflict
    
    alt T1 phát hiện slug mà T2 đang crawl (edge case)
        T1->>DB: INSERT IGNORE INTO books ...
        Note over T1: INSERT IGNORE → idempotent, không fail
        T1->>DB: INSERT IGNORE INTO chapters ...
        Note over T1: chapter-level dedup xử lý gracefully
    end
    
    T1-->>Sched: done (45 min)
    T2-->>Sched: done (3h 12min)
```

---

## Priority Queue Logic cho Tier 2

```
Candidates sorted by urgency score:

score = (hours_since_last_check / check_interval) × recency_weight

where:
  check_interval = 6h   if days_since_new_chapter <= 7   (hot)
  check_interval = 24h  if days_since_new_chapter <= 30  (warm)  
  check_interval = 72h  if days_since_new_chapter > 30   (cold ongoing)

  recency_weight = 1.5  if new chapter in last 24h (very hot)
                = 1.0   otherwise

Score > 1.0 → overdue → process first
Score < 1.0 → not yet due → skip
```

**Ví dụ** (`limit=200` candidates/run):
```
Run T+6h:
  hot stories (7 ngày):  1500 × 30% = 450 → overdue ~150  ← lấy hết
  warm stories (30 ngày): 500 × 25% = 125                  ← lấy ~50
  cold ongoing:           300 × 8%  =  24                  ← lấy hết
  Total: ~224 → cap ở 200
```

---

## Tóm tắt request budget

| Job | Tần suất | Requests/run | Requests/ngày |
|-----|----------|-------------|---------------|
| Tier 1 Listing Monitor | 24×/ngày | ~120 (fixed) | **~2.880** |
| Tier 2 Active Updater | 4×/ngày | ~200 probes + ~50 full fetches | **~1.400** |
| Tier 3 Audit | 1×/tháng | ~50 | ~2/ngày |
| **Tổng** | | | **~4.280/ngày** |
| **Cũ (recrawl_existing)** | | | **~50.000+/ngày** |
| **Giảm** | | | **~91%** |
