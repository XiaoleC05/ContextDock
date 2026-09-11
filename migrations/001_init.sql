-- ContextDock 初始化建表。
--
-- 这个文件会被挂载到容器的 /docker-entrypoint-initdb.d/ 下，
-- ⚠️ 只在**数据目录为空时执行一次**。数据卷已存在时改这个文件不会生效，
-- 需要手动执行或者删卷重建（docker compose down -v）。

-- pgvector 扩展。
--
-- ⚠️ 放在 init 脚本里，不要放到应用的 AfterConnect 里：
-- AfterConnect 会对**每条新连接**执行一次 DDL，而且要求连接用户有 CREATE 权限。
CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- 文档表
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS document (
    id         bigserial PRIMARY KEY,
    title      text        NOT NULL,
    source     text        NOT NULL DEFAULT '',
    -- 保存切分**之前**的原文。
    -- 以后调整切分参数时，只要原文还在就能重切；只存片段的话只能重新导入。
    content    text        NOT NULL,
    metadata   jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 片段表
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS chunk (
    id           bigserial PRIMARY KEY,
    document_id  bigint      NOT NULL REFERENCES document(id) ON DELETE CASCADE,
    ordinal      int         NOT NULL,
    -- rune（字符）偏移，左闭右开 —— 不是字节偏移。
    start_offset int         NOT NULL DEFAULT 0,
    end_offset   int         NOT NULL DEFAULT 0,
    content      text        NOT NULL,
    metadata     jsonb,
    -- ⚠️ 必须是 1024 维，不能是 4096。
    -- pgvector 的 HNSW 索引对 vector 类型上限是 2000 维，
    -- 4096 维连 halfvec（上限 4000）都建不了索引。
    -- 详见 docs/DESIGN.md §1。
    embedding    vector(1024),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- 按文档取片段（上下文扩展用），同时支撑 ORDER BY document_id, ordinal。
CREATE INDEX IF NOT EXISTS chunk_document_id_ordinal_idx
    ON chunk (document_id, ordinal, id);

-- 向量近邻索引。
--
-- ⚠️ 批量导入大量数据时应当**先插数据、后建索引**：
-- 已有 HNSW 索引时插入 100 万条约 1 小时，无索引只要 45 秒。
-- 本项目的导入量不大，所以建表时就建好。
--
-- 建索引时记得调大 maintenance_work_mem，否则会退化到磁盘构建，慢 10–50 倍：
--     SET maintenance_work_mem = '512MB';
CREATE INDEX IF NOT EXISTS chunk_embedding_hnsw_idx
    ON chunk USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
