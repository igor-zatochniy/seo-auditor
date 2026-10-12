package main

import (
	"container/list"
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

type graphArc struct {
	to   int64
	cost int
}

// 0-1 BFS measures clicks from the submitted root; redirects do not add a click.
func siteClickDepths(ctx context.Context, adj map[int64][]graphArc) (map[int64]int, error) {
	depths := map[int64]int{1: 0}
	queue := list.New()
	queue.PushBack(int64(1))
	for queue.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		front := queue.Front()
		id := front.Value.(int64)
		queue.Remove(front)
		for _, arc := range adj[id] {
			depth := depths[id] + arc.cost
			if old, ok := depths[arc.to]; ok && old <= depth {
				continue
			}
			depths[arc.to] = depth
			if arc.cost == 0 {
				queue.PushFront(arc.to)
			} else {
				queue.PushBack(arc.to)
			}
		}
	}
	return depths, nil
}

type siteNodeMetrics struct {
	incoming, outgoing, broken, redirects int32
	http                                  *int
}
type siteMetricEdge struct {
	from               int64
	ordinal            int
	to                 *int64
	fingerprint        [32]byte
	internal, nofollow bool
	kind               string
	http               *int
}

func readSiteMetricBatch(ctx context.Context, pool *pgxpool.Pool, cfg Config, after int64, ordinal int) ([]siteMetricEdge, error) {
	const batchSize = 8192
	var edges []siteMetricEdge
	err := withDBReadRetry(ctx, cfg, "read_site_graph_batch", func(qctx context.Context) error {
		rows, err := pool.Query(qctx, `SELECT e.from_target_id,e.ordinal,e.to_fingerprint,e.is_internal,e.nofollow,e.kind
			FROM audit_site_edges e
			WHERE e.run_id=$1 AND (e.from_target_id,e.ordinal)>($2,$3)
			ORDER BY e.from_target_id,e.ordinal LIMIT $4`, cfg.RunID, after, ordinal, batchSize)
		if err != nil {
			return err
		}
		defer rows.Close()
		batch := make([]siteMetricEdge, 0, batchSize)
		for rows.Next() {
			var e siteMetricEdge
			var fp []byte
			if err := rows.Scan(&e.from, &e.ordinal, &fp, &e.internal, &e.nofollow, &e.kind); err != nil {
				return err
			}
			if len(fp) != 32 {
				return fmt.Errorf("invalid site fingerprint length")
			}
			copy(e.fingerprint[:], fp)
			batch = append(batch, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		edges = batch
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read site graph after %d/%d: %w", after, ordinal, err)
	}
	return edges, nil
}

func finalizeSiteGraph(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	metrics := make(map[int64]*siteNodeMetrics)
	identities := make(map[[32]byte]int64)
	err := withDBReadRetry(ctx, cfg, "read_site_graph_nodes", func(qctx context.Context) error {
		rows, err := pool.Query(qctx, `SELECT n.target_id,n.url_fingerprint,r.status_code FROM audit_site_nodes n
			LEFT JOIN audit_results r USING(run_id,target_id) WHERE n.run_id=$1 ORDER BY n.target_id LIMIT $2`, cfg.RunID, maxSitePages+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var fp []byte
			m := &siteNodeMetrics{}
			if err := rows.Scan(&id, &fp, &m.http); err != nil {
				return err
			}
			if len(fp) != 32 {
				return fmt.Errorf("invalid site node fingerprint")
			}
			identities[[32]byte(fp)] = id
			metrics[id] = m
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("read site graph nodes: %w", err)
	}
	if len(metrics) > maxSitePages {
		return fmt.Errorf("site graph exceeds node budget")
	}
	if err = setSiteGraphReady(ctx, pool, cfg, false); err != nil {
		return fmt.Errorf("mark site graph incomplete: %w", err)
	}
	lastIncomingSource := make(map[int64]int64)
	var after, current int64
	ordinal, total := 0, 0
	seenDest := make(map[[32]byte]bool)
	for {
		batch, err := readSiteMetricBatch(ctx, pool, cfg, after, ordinal)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if id, ok := identities[e.fingerprint]; ok {
				e.to = &id
				e.http = metrics[id].http
			}
			total++
			if total > maxSitePages*256 {
				return fmt.Errorf("site graph exceeds edge budget")
			}
			m := metrics[e.from]
			if m == nil {
				return fmt.Errorf("site edge has no source node")
			}
			if current != e.from {
				clear(seenDest)
				current = e.from
			}
			if e.internal {
				if e.kind == "link" {
					if !seenDest[e.fingerprint] {
						m.outgoing++
						if e.http != nil && *e.http >= 400 {
							m.broken++
						}
						if e.http != nil && *e.http >= 300 && *e.http < 400 {
							m.redirects++
						}
						seenDest[e.fingerprint] = true
					}
					if e.to != nil && *e.to != e.from && lastIncomingSource[*e.to] != e.from {
						destination := metrics[*e.to]
						if destination == nil {
							return fmt.Errorf("site edge has no destination node")
						}
						destination.incoming++
						lastIncomingSource[*e.to] = e.from
					}
				}
			}
			after, ordinal = e.from, e.ordinal
		}
	}
	depths, err := siteDatabaseClickDepths(ctx, pool, cfg, identities)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(metrics))
	for id := range metrics {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for len(ids) > 0 {
		batch := ids[:min(len(ids), min(effectiveURLBatchSize(cfg), 100))]
		ids = ids[len(batch):]
		values, incoming, outgoing, broken, redirects := make([]int32, len(batch)), make([]int32, len(batch)), make([]int32, len(batch)), make([]int32, len(batch)), make([]int32, len(batch))
		for i, id := range batch {
			values[i] = -1
			if depth, ok := depths[id]; ok {
				values[i] = int32(depth)
			}
			m := metrics[id]
			incoming[i], outgoing[i], broken[i], redirects[i] = m.incoming, m.outgoing, m.broken, m.redirects
		}
		err = withDBMutationRetry(ctx, cfg, "save_site_graph_batch", func(qctx context.Context) error {
			tx, err := pool.Begin(qctx)
			if err != nil {
				return err
			}
			defer rollbackSiteTransaction(tx, cfg)
			if err = lockSiteOwner(qctx, tx, cfg); err != nil {
				return err
			}
			tag, err := tx.Exec(qctx, `UPDATE audit_site_nodes n SET crawl_depth=NULLIF(v.depth,-1),internal_inlinks_count=v.incoming,
				internal_outlinks_count=v.outgoing,broken_internal_links=v.broken,redirecting_internal_links=v.redirects,
				orphan_candidate=(n.target_id<>1 AND n.in_sitemap AND v.depth=-1 AND v.incoming=0)
				FROM unnest($2::BIGINT[],$3::INT[],$4::INT[],$5::INT[],$6::INT[],$7::INT[]) v(id,depth,incoming,outgoing,broken,redirects)
				WHERE n.run_id=$1 AND n.target_id=v.id`, cfg.RunID, batch, values, incoming, outgoing, broken, redirects)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != int64(len(batch)) {
				return fmt.Errorf("site graph node set changed")
			}
			return tx.Commit(qctx)
		})
		if err != nil {
			return fmt.Errorf("save site graph metrics from target %d: %w", batch[0], err)
		}
	}
	return setSiteGraphReady(ctx, pool, cfg, true)
}

// В RAM залишаються відстані та frontier, а не до 5 млн ребер графа.
// Кеш лише чотирьох пакетів також не створює SQL-запит на кожен крок довгого ланцюга.
func siteDatabaseClickDepths(ctx context.Context, pool *pgxpool.Pool, cfg Config, identities map[[32]byte]int64) (map[int64]int, error) {
	const sourceBatchSize = 32
	depths := map[int64]int{1: 0}
	queue := []int64{1}
	queued := map[int64]bool{1: true}
	cache := map[int64]map[int64][]graphArc{}
	order := []int64{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := queue[0]
		queue = queue[1:]
		queued[id] = false
		block := (id-1)/sourceBatchSize*sourceBatchSize + 1
		arcs, loaded := cache[block]
		if !loaded {
			err := withDBReadRetry(ctx, cfg, "site_depth_frontier", func(qctx context.Context) error {
				// Ідентичності вже належать snapshot; JOIN не потрібний для кожного пакета ребер.
				rows, err := pool.Query(qctx, `SELECT e.from_target_id,e.to_fingerprint,CASE WHEN e.kind='redirect' THEN 0 ELSE 1 END
				FROM audit_site_edges e
				WHERE e.run_id=$1 AND e.from_target_id>=$2 AND e.from_target_id<$2+$3 AND e.is_internal AND NOT e.nofollow
				ORDER BY e.from_target_id,e.ordinal LIMIT $4`, cfg.RunID, block, sourceBatchSize, sourceBatchSize*256+1)
				if err != nil {
					return err
				}
				defer rows.Close()
				arcs = map[int64][]graphArc{}
				count := 0
				for rows.Next() {
					var from int64
					var fingerprint []byte
					var a graphArc
					if err := rows.Scan(&from, &fingerprint, &a.cost); err != nil {
						return err
					}
					if len(fingerprint) != 32 {
						return fmt.Errorf("invalid site edge fingerprint")
					}
					count++
					if count > sourceBatchSize*256 {
						return fmt.Errorf("site depth batch exceeds edge budget")
					}
					to, known := identities[[32]byte(fingerprint)]
					if !known {
						continue
					}
					a.to = to
					arcs[from] = append(arcs[from], a)
				}
				return rows.Err()
			})
			if err != nil {
				return nil, fmt.Errorf("read site depth block %d: %w", block, err)
			}
			if len(order) == 4 {
				delete(cache, order[0])
				order = order[1:]
			}
			cache[block] = arcs
			order = append(order, block)
		}
		for _, a := range arcs[id] {
			depth := depths[id] + a.cost
			if old, ok := depths[a.to]; ok && old <= depth {
				continue
			}
			depths[a.to] = depth
			if len(depths) > maxSitePages {
				return nil, fmt.Errorf("site depth exceeds node budget")
			}
			if !queued[a.to] {
				queue = append(queue, a.to)
				queued[a.to] = true
			}
		}
	}
	return depths, nil
}

func setSiteGraphReady(ctx context.Context, pool *pgxpool.Pool, cfg Config, ready bool) error {
	return withDBMutationRetry(ctx, cfg, "set_site_graph_ready", func(qctx context.Context) error {
		tx, err := pool.Begin(qctx)
		if err != nil {
			return err
		}
		defer rollbackSiteTransaction(tx, cfg)
		if err = lockSiteOwner(qctx, tx, cfg); err != nil {
			return err
		}
		if _, err = tx.Exec(qctx, `UPDATE audit_site_crawls SET graph_ready=$2 WHERE run_id=$1`, cfg.RunID, ready); err != nil {
			return err
		}
		return tx.Commit(qctx)
	})
}
