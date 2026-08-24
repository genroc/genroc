-- Every owner of externalized values lists them the same way: an `objects` column beside the
-- data, holding [{path, ref, size}] with paths rooted at that owner's value.
-- specs/object-store.md.
--
-- The references were embedded per slot, in a model.Envelope {data, refs} wrapper per column. That
-- made "what does this owner reference" a question you answered by knowing the storage layout of
-- every column -- which is what `gc_chaos_test.ts` does by hand, reconstructing refs out of
-- input_data, output_data, outputs_data.items and process_logs.data before it can compare them
-- against object_refs. One column per owner makes that comparison a read, and the same shape the
-- API already puts on the wire.
--
-- No conversion. The value columns change meaning (the envelope wrapper goes, so an old row's
-- {"data":…} would decode AS the value), and genroc has one user and no deployment -- the database
-- is wiped by hand. If that stops being true this needs a real conversion, not a wipe.
ALTER TABLE process_instances ADD COLUMN objects TEXT NOT NULL DEFAULT '';
ALTER TABLE process_logs ADD COLUMN objects TEXT NOT NULL DEFAULT '';
