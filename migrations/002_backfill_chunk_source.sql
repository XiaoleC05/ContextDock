-- 把 document.source 回填进每个 chunk 的 metadata。
--
-- 背景：MCP 检索结果里的溯源字段 `source` 曾经是个**空壳**——
-- DTO 里声明了、jsonschema 里也写了描述（Agent 在 tools/list 里看得见），
-- 但 toResultItem 从不给它赋值。根因是 Source 挂在 document 上，
-- 而检索路径里走的只有 chunk，拿不到它。
--
-- 修复办法是在 ingest 阶段把 source 下沉进 chunk.metadata。
-- 但那**只对新导入的文档生效**：修复之前入库的片段，
-- metadata 里压根没有这个键，检索出来 source 就是空的。
-- 这个脚本负责把历史数据补齐。
--
-- ⚠️ 必须**手动执行**，不会被自动跑。
-- /docker-entrypoint-initdb.d/ 里的脚本只在数据目录为空时执行一次，
-- 数据卷已存在时新增脚本不会生效（001_init.sql 里记了同一件事）。
--
--     docker exec -i contextdock-pg psql -U postgres -d contextdock \
--         < migrations/002_backfill_chunk_source.sql
--
-- 幂等：末尾的 IS DISTINCT FROM 保证重复执行不产生任何变化。
--
-- 会跳过的情况：document.source 为空串的文档。
-- 那些片段保持"没有 source 键"，和 ingest 里 `if doc.Source != ""` 的
-- 行为一致——空值和缺失混在一起，将来做迁移就分不清了。

UPDATE chunk AS c
SET metadata = coalesce(c.metadata, '{}'::jsonb)
               || jsonb_build_object('source', d.source)
FROM document AS d
WHERE c.document_id = d.id
  AND d.source <> ''
  AND (c.metadata ->> 'source') IS DISTINCT FROM d.source;
