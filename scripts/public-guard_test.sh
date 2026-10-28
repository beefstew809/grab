#!/bin/sh
# Tests for public-guard.sh against throwaway repositories.
# Usage: sh scripts/public-guard_test.sh   (exits non-zero on any failure)
set -u
GUARD=$(cd "$(dirname "$0")" && pwd)/public-guard.sh
fails=0

newrepo() {
	d=$(mktemp -d)
	git -C "$d" init -q
	git -C "$d" config user.name Test
	git -C "$d" config user.email test@example.com
	git -C "$d" config tag.gpgSign false
	git -C "$d" config commit.gpgSign false
	echo "$d"
}

commit() { # repo message [env assignments...]
	r=$1; m=$2; shift 2
	# Neutral content: cases put denied strings in metadata unless they mean the tree.
	echo x >> "$r/f"
	git -C "$r" add f
	env "$@" git -C "$r" commit -q -m "$m"
}

# expect name wantcode wantsubstr notsubstr cmd...
expect() {
	name=$1; want=$2; has=$3; hasnot=$4; shift 4
	out=$("$@" 2>&1); code=$?
	ok=1
	[ "$code" = "$want" ] || ok=0
	if [ -n "$has" ]; then case $out in *"$has"*) ;; *) ok=0 ;; esac; fi
	if [ -n "$hasnot" ]; then case $out in *"$hasnot"*) ok=0 ;; esac; fi
	if [ $ok = 1 ]; then echo "PASS $name"; else
		echo "FAIL $name (exit $code, want $want)"; echo "$out" | sed 's/^/    /'; fails=$((fails + 1)); fi
}

r=$(newrepo); commit "$r" clean
expect case_clean 0 "" "" env GRAB_DENYLIST=blocked.example sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" one GIT_AUTHOR_EMAIL=x@blocked.example
expect case_author_email 1 "author email" "blocked.example" env GRAB_DENYLIST=blocked.example sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" one GIT_COMMITTER_EMAIL=x@blocked.example
expect case_committer_email 1 "committer email" "blocked.example" env GRAB_DENYLIST=blocked.example sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" "has BLOCKED word"
expect case_message 1 "message" "BLOCKED" env GRAB_DENYLIST=blocked sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" clean; git -C "$r" tag -a v1.0.0 -m "see blocked.example"
expect case_tag_message 1 "tag v1.0.0: message" "blocked.example" env GRAB_DENYLIST=blocked.example sh -c "cd $r && sh $GUARD --all v1.0.0"

r=$(newrepo); commit "$r" clean; git -C "$r" tag -a blocked-1 -m "fine"
expect case_tag_name 1 ": name" "blocked-1" env GRAB_DENYLIST=blocked sh -c "cd $r && sh $GUARD --all blocked-1"

r=$(newrepo); commit "$r" clean
expect case_empty_denylist_fails 2 "" "" env GRAB_DENYLIST=' , ' sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" "first BLOCKED"; commit "$r" second
expect case_range_clean_tail 0 "" "" env GRAB_DENYLIST=blocked sh -c "cd $r && sh $GUARD HEAD~1..HEAD"
expect case_range_all_dirty 1 "message" "" env GRAB_DENYLIST=blocked sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" clean
expect case_bad_range 2 "" "" env GRAB_DENYLIST=blocked sh -c "cd $r && sh $GUARD nosuch..HEAD"

# A denied string added and removed again within the pushed commits.
r=$(newrepo); echo "see blocked.example" > "$r/leak.txt"; git -C "$r" add leak.txt; git -C "$r" commit -q -m add
git -C "$r" rm -q leak.txt; git -C "$r" commit -q -m remove
expect case_tree_intermediate 1 "tree" "blocked.example" env GRAB_DENYLIST=blocked.example sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); echo fine > "$r/blocked-notes.txt"; git -C "$r" add blocked-notes.txt; git -C "$r" commit -q -m add
expect case_path_name 1 "file names" "blocked-notes" env GRAB_DENYLIST=blocked sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" one GIT_AUTHOR_EMAIL=someone@elsewhere.example
expect case_email_not_allowed 1 "author email not allowed" "elsewhere" env GRAB_DENYLIST=unused GRAB_ALLOWED_EMAILS=test@example.com sh -c "cd $r && sh $GUARD --all"

r=$(newrepo); commit "$r" one; git -C "$r" tag -a v1.0.0 -m ok
expect case_email_allowed 0 "" "" env GRAB_DENYLIST=unused GRAB_ALLOWED_EMAILS='test@example.com, bot@renovateapp.com' sh -c "cd $r && sh $GUARD --all v1.0.0"

r=$(newrepo); commit "$r" one; GIT_COMMITTER_EMAIL=tagger@elsewhere.example git -C "$r" tag -a v1.0.0 -m ok
expect case_tagger_not_allowed 1 "tagger email not allowed" "elsewhere" env GRAB_DENYLIST=unused GRAB_ALLOWED_EMAILS=test@example.com sh -c "cd $r && sh $GUARD --all v1.0.0"

[ $fails = 0 ] || { echo "$fails case(s) failed"; exit 1; }
echo "all cases passed"
