/* Euchre web client — vanilla JS, no framework, no build step, offline.
 * Renders full-state snapshots pushed over SSE (GET /api/events) and posts
 * actions to /api/bid, /api/play, /api/discard, /api/new.
 * Structured around a pure-ish render(state) so the render path can be
 * exercised headlessly against a DOM stub (see mock test). */
(function () {
  'use strict';

  var SEAT_LABEL = { north: 'North', east: 'East', south: 'You', west: 'West' };
  var SUITS = ['♣', '♦', '♥', '♠'];
  var SUIT_TO_NAME = { '♣': 'clubs', '♦': 'diamonds', '♥': 'hearts', '♠': 'spades' };
  var NAME_TO_SUIT = { clubs: '♣', diamonds: '♦', hearts: '♥', spades: '♠' };
  var CODE_TO_SUIT = { C: '♣', D: '♦', H: '♥', S: '♠' };
  var RED = { '♥': true, '♦': true };
  var BID_TAG = { order_up: '↑ order-up', pass: '– pass', alone: '★ alone' };

  var state = null;
  var pending = false;
  var lastError = null;
  var es = null;
  var reconnectTimer = null;

  /* ---------- helpers ---------- */

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function cap(s) { s = s || ''; return s.charAt(0).toUpperCase() + s.slice(1); }

  function suitOf(card) { return card ? card.charAt(card.length - 1) : ''; }
  function rankOf(card) { return card ? card.slice(0, -1) : ''; }

  /* ---------- pure HTML builders ---------- */

  function cardHTML(card, cls, attrs) {
    if (!card) {
      return '<div class="card card-empty" aria-hidden="true">·</div>';
    }
    var color = RED[suitOf(card)] ? 'red' : 'black';
    return '<div class="card ' + color + (cls ? ' ' + cls : '') + '"' +
      (attrs ? ' ' + attrs : '') + '>' +
      '<span class="card-rank">' + esc(rankOf(card)) + '</span>' +
      '<span class="card-suit">' + suitOf(card) + '</span></div>';
  }

  function suitSpan(card) {
    if (!card) return '';
    var s = suitOf(card);
    return '<span class="' + (RED[s] ? 'suit-red' : 'suit-black') + '">' + s + '</span>';
  }

  function bidTag(bid) {
    if (!bid) return '';
    if (BID_TAG[bid]) return '<span class="bid-tag">' + BID_TAG[bid] + '</span>';
    var sym = NAME_TO_SUIT[bid] || CODE_TO_SUIT[bid] || (SUIT_TO_NAME[bid] ? bid : null);
    if (sym) return '<span class="bid-tag">call ' + sym + '</span>';
    return '<span class="bid-tag">' + esc(bid) + '</span>';
  }

  function seatHTML(st, seat) {
    var info = (st.seatInfo && st.seatInfo[seat]) || {};
    var isTurn = !!info.isTurn;
    var bits = [];

    if (info.isDealer) bits.push('<span class="dealer-btn" title="Dealer">D</span>');

    bits.push('<div class="seat-name">' + SEAT_LABEL[seat] +
      (seat === 'south' ? ' <span class="chip you">you</span>' : ' <span class="chip">bot</span>') +
      '</div>');

    var tag = bidTag(info.lastBid);
    bits.push('<div class="seat-bid">' + (tag || '<span class="quiet">·</span>') + '</div>');

    var n = typeof info.cardsLeft === 'number' ? info.cardsLeft : null;
    var pips = '';
    if (n !== null) for (var i = 0; i < Math.min(n, 6); i++) pips += '●';
    bits.push('<div class="seat-cards"><span class="pips">' + pips + '</span> ' +
      '<span class="cards-left">' + (n === null ? '–' : n) + '</span></div>');

    var tags = [];
    if (st.maker === seat) tags.push('maker');
    if (st.alone === seat) tags.push('alone ★');
    if (tags.length) {
      bits.push('<div class="seat-tags">' + tags.map(function (t) {
        return '<span class="chip tag">' + esc(t) + '</span>';
      }).join('') + '</div>');
    }

    if (isTurn) bits.push('<div class="turn-note">' + (seat === 'south' ? 'your turn' : 'thinking…') + '</div>');

    return '<div class="seat' + (isTurn ? ' turn' : '') + '">' + bits.join('') + '</div>';
  }

  function statusHTML(st) {
    var trump = st.trump
      ? '<span class="trump-badge">★ <span class="' + (RED[st.trump] ? 'suit-red' : 'suit-black') + '">' + st.trump + '</span></span>'
      : '<span class="quiet">—</span>';
    var t = st.tricks || {};
    var sc = st.score || {};
    return 'Hand <b>' + esc(st.hand == null ? '–' : st.hand) + '</b>' +
      ' · Dealer <b>' + esc(cap(st.dealer)) + '</b>' +
      ' · Trump ' + trump +
      ' · Tricks <b>NS ' + (t.ns || 0) + '</b> / <b>EW ' + (t.ew || 0) + '</b>' +
      ' · Score <b>NS ' + (sc.ns || 0) + '</b> — <b>EW ' + (sc.ew || 0) + '</b> <span class="quiet">(to 10)</span>' +
      (st.maker ? ' · Maker <b>' + esc(cap(st.maker)) + (st.alone ? ' (alone)' : '') + '</b>' : '');
  }

  function trickHTML(st) {
    var tr = st.trick || {};
    return ['north', 'west', 'east', 'south'].map(function (seat) {
      var cls = seat === 'north' ? 'slot-north'
        : seat === 'south' ? 'slot-south'
        : seat === 'west' ? 'slot-west'
        : 'slot-east';
      return '<div class="trick-slot ' + cls + '">' + cardHTML(tr[seat], 'played') + '</div>';
    }).join('');
  }

  function handHTML(st) {
    var hand = st.yourHand || [];
    var legal = st.legal || [];
    var isDiscard = st.phase === 'discard';
    var isPlay = st.phase === 'play';
    return hand.map(function (card) {
      var isLegal = legal.indexOf(card) !== -1 && (isDiscard || isPlay);
      var cls = 'tile' + (isLegal ? ' clickable' : ' dim');
      var attrs = isLegal
        ? 'data-action="' + (isDiscard ? 'discard' : 'play') + '" data-card="' + esc(card) + '"'
        : '';
      return cardHTML(card, cls, attrs);
    }).join('');
  }

  function actionHTML(st) {
    var out = '';
    if (lastError) out += '<div class="err">⚠ ' + esc(lastError) + '</div>';
    if (st.phase === 'game_over') return out; /* overlay covers it */

    var bid = st.bidding || {};
    if (bid.active) {
      if (bid.promptTo === 'south') {
        var alone = '<label class="alone"><input type="checkbox" id="alone-check"> go alone</label>';
        if (st.bidRound === 1) {
          out += '<button class="btn" data-action="bid" data-decision="order_up">Order up ' +
            cardHTML(st.topCard, 'mini') + '</button>';
        } else {
          var skip = suitOf(st.topCard || '');
          out += SUITS.filter(function (s) { return s !== skip; }).map(function (s) {
            return '<button class="btn" data-action="bid" data-decision="' + SUIT_TO_NAME[s] +
              '">Call ' + s + '</button>';
          }).join('');
        }
        out += alone + '<button class="btn pass" data-action="bid" data-decision="pass">Pass</button>';
      } else {
        out += '<span class="wait">Waiting for ' + esc(cap(bid.promptTo)) + ' to bid…</span>';
      }
      return out;
    }

    if (st.phase === 'discard') {
      out += '<span class="wait">You picked up ' + cardHTML(st.topCard, 'mini') +
        ' — discard one card below</span>';
      return out;
    }

    if (st.phase === 'hand_over') {
      var t = st.tricks || {};
      var sc = st.score || {};
      out += '<span class="wait">Hand ' + esc(st.hand == null ? '' : st.hand) +
        ' over — tricks NS ' + (t.ns || 0) + ' · EW ' + (t.ew || 0) +
        ' — score NS ' + (sc.ns || 0) + ' · EW ' + (sc.ew || 0) + '. Next hand starting…</span>';
      return out;
    }

    if (st.phase === 'play') {
      if (st.turn === 'south') {
        out += '<span class="wait yourturn">Your turn — play a highlighted card</span>';
      } else if (st.turn) {
        out += '<span class="wait">Waiting for ' + esc(cap(st.turn)) + '…</span>';
      } else {
        out += '<span class="wait">Trick in progress…</span>';
      }
      return out;
    }

    return out;
  }

  function tipsHTML(st) {
    var tips = st.tips || [];
    return '<h3>Coach</h3>' + (tips.length
      ? '<ul>' + tips.map(function (t) { return '<li>💡 ' + esc(t) + '</li>'; }).join('') + '</ul>'
      : '<div class="quiet">no advice yet</div>');
  }

  function logHTML(st) {
    var log = st.log || [];
    return '<h3>Log</h3><div class="log-lines">' +
      log.slice(-8).map(function (l) { return '<div>' + esc(l) + '</div>'; }).join('') +
      '</div>';
  }

  function overlayHTML(st) {
    var go = st.gameOver;
    if (st.phase !== 'game_over' || !go) return '';
    var winner = go.winner === 'ns' ? 'North–South'
      : go.winner === 'ew' ? 'East–West'
      : esc(go.winner);
    var sc = go.score || st.score || {};
    return '<div class="box">' +
      '<div class="over-title">' + winner + ' wins!</div>' +
      '<div class="over-score">Final score — NS ' + (sc.ns || 0) + ' · EW ' + (sc.ew || 0) + '</div>' +
      '<button class="btn" data-action="new">New game</button></div>';
  }

  /* ---------- render ---------- */

  function set(id, html) {
    var el = document.getElementById(id);
    if (el) el.innerHTML = html;
  }

  function render(st) {
    st = st || {};
    state = st;

    set('status-info', statusHTML(st));
    ['north', 'west', 'east', 'south'].forEach(function (seat) {
      set('seat-' + seat, seatHTML(st, seat));
    });
    set('center', '<div class="trick-grid">' + trickHTML(st) + '</div>');
    set('hand', handHTML(st));

    var act = document.getElementById('action');
    if (act) {
      act.innerHTML = actionHTML(st);
      act.className = pending ? 'disabled' : '';
    }

    set('tips', tipsHTML(st));
    set('log', logHTML(st));

    var ov = document.getElementById('overlay');
    if (ov) {
      var oh = overlayHTML(st);
      ov.innerHTML = oh;
      ov.className = oh ? 'show' : '';
    }
    return st;
  }

  /* ---------- networking ---------- */

  function post(path, body) {
    pending = true;
    if (state) render(state);
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {})
    })
      .then(function (res) {
        return res.json().catch(function () { return {}; })
          .then(function (j) { return { ok: res.ok, status: res.status, body: j }; });
      })
      .then(function (r) {
        if (r.ok) {
          lastError = null;
          if (r.body && r.body.phase) state = r.body;
        } else {
          lastError = (r.body && r.body.error) || ('error ' + r.status);
        }
      })
      .catch(function (err) {
        lastError = String((err && err.message) || err);
      })
      .then(function () {
        pending = false;
        if (state) render(state);
      });
  }

  function onClick(e) {
    var el = e.target;
    while (el && el !== document && !(el.getAttribute && el.getAttribute('data-action'))) {
      el = el.parentNode;
    }
    if (!el || el === document || pending) return;
    var action = el.getAttribute('data-action');
    if (action === 'play') {
      post('/api/play', { card: el.getAttribute('data-card') });
    } else if (action === 'discard') {
      post('/api/discard', { card: el.getAttribute('data-card') });
    } else if (action === 'bid') {
      var chk = document.getElementById('alone-check');
      post('/api/bid', {
        decision: el.getAttribute('data-decision'),
        goingAlone: !!(chk && chk.checked)
      });
    } else if (action === 'new') {
      post('/api/new', {});
    }
  }

  function setConn(on) {
    var d = document.getElementById('conn-dot');
    if (d) d.className = 'dot ' + (on ? 'on' : 'off');
  }

  function scheduleReconnect() {
    if (reconnectTimer) return;
    reconnectTimer = setTimeout(function () {
      reconnectTimer = null;
      connect();
    }, 2000);
  }

  function connect() {
    try {
      es = new EventSource('/api/events');
    } catch (e) {
      setConn(false);
      scheduleReconnect();
      return;
    }
    es.onopen = function () { setConn(true); };
    es.onmessage = function (ev) {
      setConn(true);
      var st;
      try { st = JSON.parse(ev.data); } catch (e) { return; }
      lastError = null;
      render(st || {});
    };
    es.onerror = function () {
      setConn(false);
      try { es.close(); } catch (e) { /* already gone */ }
      es = null;
      scheduleReconnect();
    };
  }

  function init() {
    document.addEventListener('click', onClick);
    connect();
  }

  /* ---------- export / bootstrap ---------- */

  var api = {
    render: render,
    cardHTML: cardHTML,
    statusHTML: statusHTML,
    seatHTML: seatHTML,
    trickHTML: trickHTML,
    handHTML: handHTML,
    actionHTML: actionHTML,
    tipsHTML: tipsHTML,
    logHTML: logHTML,
    overlayHTML: overlayHTML
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api; /* headless test hook */
    return;
  }

  if (typeof window !== 'undefined' && typeof document !== 'undefined') {
    window.__euchre = api; /* debug/testing hook */
    init();
  }
})();
