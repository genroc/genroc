-- One set of columns per error DIRECTION.
--
-- error_internal is the error the instance CAUGHT. It belongs to its state at the task it
-- stopped on -- what a layer describes and an upgrade validates -- so a concluding fault must
-- not edit it. (This is the column 019 called error_data.)
--
-- error_code / error_message / error_data are the error it REPORTS. Three plain columns and
-- not one JSON object, because error_code is filtered on: a code buried in a blob cannot be
-- indexed or matched in SQL. Only the payload needs a value column of its own -- it is
-- arbitrarily large and takes the same object-store cut every other value slot gets.
ALTER TABLE process_instances RENAME COLUMN error_data TO error_internal;
ALTER TABLE process_instances RENAME COLUMN error TO error_message;
ALTER TABLE process_instances ADD COLUMN error_data TEXT NOT NULL DEFAULT '';
