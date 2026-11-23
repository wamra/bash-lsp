import json
names=('bls','shuck','bashd','bash-lsp')
R=[json.load(open(f'results/{n}.json')) for n in names]; A=[json.load(open(f'results/{n}-ann.json')) for n in names]
M=json.load(open('results/mvdan.json'))
ids=list(R[0]['results'])
def cell(r,a,c):
    x='pass' if r['results'][c][0] else 'FAIL'; y='pass' if a['results'][c][0] else 'FAIL'
    return x if x==y else f'{x} / {y}'
print('Server cells: plain corpus; "x / y" where the annotated corpus (`# shellcheck source=` directives) differs.')
print('Last column is not a server: it says whether the mvdan/sh tree alone contains the facts the case needs ("facts" = yes, "MISSING" = no). Resolving them is the layer bash-lsp adds.\n')
print('| Case | What is tested | '+' | '.join(r['name'] for r in R)+' | mvdan/sh 3.14.1, parser only |'); print('|---|---|'+'---|'*(len(R)+1))
for c in ids: print('|',c,'|',R[0]['results'][c][1],'|',' | '.join(cell(r,a,c) for r,a in zip(R,A)),'|','facts' if M['results'][c][0] else 'MISSING','|')
sc=lambda r: sum(v[0] for v in r['results'].values())
print('| **Score, plain** | of %d | '%len(ids)+' | '.join(str(sc(r)) for r in R)+' | %d have the facts |'%sc(M))
print('| **Score, annotated** | of %d | '%len(ids)+' | '.join(str(sc(a)) for a in A)+' | |')
print('\n| Timing, small corpus | '+' | '.join(r['name'] for r in R)+' |\n|---|'+'---|'*len(R))
for k,l in (('initialize_ms','Initialise, ms'),('definition_ms_median','Definition, warm median, ms'),('rss_mb','Memory, MB')): print('|',l,'|',' | '.join(str(r['timings'][k]) for r in R),'|')
S=[json.load(open(f'results/scale-{n}.json')) for n in names]; big=M['big']
print('\n| One 561 KB script, 2,000 functions | '+' | '.join(x['name'] for x in S)+' | mvdan/sh parse only |\n|---|'+'---|'*(len(S)+1))
def g(x,k): return str(x[k]) if k in x else 'exited (message too long for its reader)'
print('| Open to first symbols, ms |',' | '.join(g(x,'open_to_symbols_ms') for x in S),'| %.0f |'%big['parse_ms'])
print('| Edit to refreshed symbols, ms |',' | '.join(g(x,'edit_to_symbols_ms') if 'error' not in x else '-' for x in S),'| %.0f |'%big['parse_ms'])
print('| Definition after the edit, ms |',' | '.join(g(x,'definition_ms') if 'error' not in x else '-' for x in S),'| - |')
print('| Memory, MB |',' | '.join(g(x,'rss_mb') if 'error' not in x else '-' for x in S),'| - |')
print('\n| Capability advertised | '+' | '.join(r['name'] for r in R)+' |\n|---|'+'---|'*len(R))
for k in R[0]['caps']: print('|',k,'|',' | '.join('yes' if r['caps'][k] else '-' for r in R),'|')
