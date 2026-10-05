-- Admission counters share the source transaction. Concurrent writers cannot
-- oversubscribe a limit, and rollback cannot leak a reservation.
CREATE TABLE work_capacity (
    name text PRIMARY KEY CHECK(name IN ('publication','dead_letters')),
    used bigint NOT NULL CHECK(used>=0),
    maximum bigint NOT NULL CHECK(maximum BETWEEN 1 AND 1000000),
    CHECK(used<=maximum)
);
INSERT INTO work_capacity(name,used,maximum) VALUES
('publication',(SELECT count(*) FROM outbox WHERE published_at IS NULL),200000),
('dead_letters',(SELECT count(*) FROM work_dead_letters),10000);

CREATE FUNCTION adjust_work_capacity(queue_name text,delta bigint) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    UPDATE work_capacity SET used=used+delta WHERE name=queue_name AND used+delta BETWEEN 0 AND maximum;
    IF NOT FOUND THEN
        IF delta>0 THEN RAISE EXCEPTION 'durable work capacity reached' USING ERRCODE='SY001';
        ELSE RAISE EXCEPTION 'durable work counter invariant failed'; END IF;
    END IF;
END;
$$;
CREATE FUNCTION count_publication_work() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE delta bigint:=0;
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.published_at IS NULL THEN delta:=1; END IF;
    ELSIF TG_OP='DELETE' THEN
        IF OLD.published_at IS NULL THEN delta:=-1; END IF;
    ELSE
        IF OLD.published_at IS NULL AND NEW.published_at IS NOT NULL THEN delta:=-1;
        ELSIF OLD.published_at IS NOT NULL AND NEW.published_at IS NULL THEN delta:=1; END IF;
    END IF;
    IF delta<>0 THEN PERFORM adjust_work_capacity('publication',delta); END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER publication_capacity AFTER INSERT OR UPDATE OR DELETE ON outbox FOR EACH ROW EXECUTE FUNCTION count_publication_work();
CREATE FUNCTION count_dead_letter_work() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN PERFORM adjust_work_capacity('dead_letters',1);
    ELSE PERFORM adjust_work_capacity('dead_letters',-1); END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER dead_letter_capacity AFTER INSERT OR DELETE ON work_dead_letters FOR EACH ROW EXECUTE FUNCTION count_dead_letter_work();
CREATE FUNCTION prevent_work_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'work queues require bounded row cleanup'; END;
$$;
CREATE TRIGGER outbox_no_truncate BEFORE TRUNCATE ON outbox EXECUTE FUNCTION prevent_work_truncate();
CREATE TRIGGER dead_letters_no_truncate BEFORE TRUNCATE ON work_dead_letters EXECUTE FUNCTION prevent_work_truncate();
