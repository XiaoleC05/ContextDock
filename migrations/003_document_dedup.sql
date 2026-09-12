-- 003: 文档去重（#55）
--
-- 给 document 加两个用于去重的列，并保证去重键唯一。
--
-- 背景：同一份内容导入两次，库里会出现两份一模一样的文档，
-- 检索时两条完全相同的结果并排返回，用户/Agent 无从分辨。
-- 参数扫描反复重灌语料时这个问题会迅速把库搞脏。
--
-- ⚠️ 这个脚本是**幂等**的（IF NOT EXISTS / WHERE 条件），可以重复执行。

ALTER TABLE document ADD COLUMN IF NOT EXISTS content_hash text NOT NULL DEFAULT '';
ALTER TABLE document ADD COLUMN IF NOT EXISTS dedup_key    text NOT NULL DEFAULT '';

-- 存量数据的 backfill。
--
-- ⚠️ 这里刻意给存量文档发一个**永远匹配不上新导入**的键（"legacy:<id>"），
-- 而不是按内容重算。两个理由：
--
--   1. 按内容重算需要 extension 算 sha256，而为了一个一次性回填
--      引入 pgcrypto 不划算；在应用层重算则要读全表 content。
--   2. 更重要的是**去重的方向**：存量数据没有经过去重，可能本来就有重复。
--      给它们发唯一的 legacy 键，等于宣告"这批不动"——之后重新导入
--      同一份文档会新增一份，而不是替换掉一个不确定是哪份的旧记录。
--
-- 代价是：存量文档重新导入一次会多出一份，需要人工清理一次。
-- 这比"自动替换掉一份不确定的记录"安全。
UPDATE document
SET dedup_key = 'legacy:' || id
WHERE dedup_key = '';

-- 唯一索引：去重键相同就是同一份文档，库层面直接拦住。
--
-- 放在应用层判断是不够的：并发导入两次同一份文档时，
-- 两个事务都会查不到、然后都插入，最后库里还是两份。
--
-- ⚠️ 是**部分索引**（WHERE dedup_key <> ''）。原因：
-- 去重是 ingest 层的职责，而 store 是更下面的一层，
-- 有人（比如集成测试）会直接调 SaveDocument 传一个没算过去重键的文档。
-- 不加这个条件的话，第二篇这样的文档会撞唯一约束而插入失败——
-- 而它根本不该受去重规则的约束。
--
-- 条件写成 `<> ''` 而不是 `IS NOT NULL`：列是 NOT NULL DEFAULT ''，
-- 空串是这里的"没有键"，不是 NULL。
DROP INDEX IF EXISTS document_dedup_key_idx;
CREATE UNIQUE INDEX IF NOT EXISTS document_dedup_key_idx
    ON document (dedup_key) WHERE dedup_key <> '';
