// Lobby: room list, create/join rooms
const API = '/api/rooms';
const NAME_STORAGE_KEY = 'pong_player_name';
const ROOM_SEQ_STORAGE_KEY = 'pong_room_seq';

// A ?diag=1 lobby URL enables the realpath diagnostics on every page it opens
// (the game page reads the same localStorage flag), so latency hunting does
// not require editing URLs on each navigation.
if (new URLSearchParams(window.location.search).get('diag') === '1') {
    try {
        localStorage.setItem('pong_diag', '1');
    } catch { /* diagnostics are optional */ }
}

let playerName = '';

function setSavedMode(nameInput, btnEdit, btnSave, status) {
    const value = playerName.trim();
    nameInput.value = value;
    nameInput.readOnly = true;
    btnEdit.disabled = false;
    btnSave.disabled = false;
    status.textContent = value ? `You play as “${value}”. Edit or save to change it.` : '';
}

function setEditingMode(nameInput, btnEdit, btnSave) {
    nameInput.readOnly = false;
    btnEdit.disabled = true;
    btnSave.disabled = false;
}

document.addEventListener('DOMContentLoaded', () => {
    const nameInput = document.getElementById('playerName');
    const btnEdit = document.getElementById('btnEditName');
    const btnSave = document.getElementById('btnSaveName');
    const nameStatus = document.getElementById('nameStatus');

    const saved = localStorage.getItem(NAME_STORAGE_KEY);
    if (saved && saved.trim()) {
        playerName = saved.trim();
        setSavedMode(nameInput, btnEdit, btnSave, nameStatus);
    } else {
        setEditingMode(nameInput, btnEdit, btnSave);
    }

    // Editing starts fresh from the current value; nothing is persisted until
    // the user explicitly saves.
    btnEdit.addEventListener('click', () => {
        setEditingMode(nameInput, btnEdit, btnSave);
        nameInput.focus();
    });

    btnSave.addEventListener('click', () => {
        playerName = nameInput.value.trim();
        if (!playerName) {
            showError('A display name is required before you can play.');
            nameInput.focus();
            return;
        }
        localStorage.setItem(NAME_STORAGE_KEY, playerName);
        setSavedMode(nameInput, btnEdit, btnSave, nameStatus);
        nameStatus.textContent = `Name saved. You will play as “${playerName}”.`;
    });

    nameInput.addEventListener('input', () => {
        playerName = nameInput.value.trim();
    });

    document.getElementById('btnNewRoom').addEventListener('click', createRoom);
    document.getElementById('btnAI').addEventListener('click', playAgainstComputer);
    refreshRooms();
    setInterval(refreshRooms, 3000);
});

async function refreshRooms() {
    try {
        const res = await fetch(API);
        const rooms = await res.json();
        renderRooms(rooms);
        document.getElementById('noRooms').classList.toggle('hidden', rooms.length > 0);
    } catch (e) {
        console.error('Failed to fetch rooms', e);
    }
}

function renderRooms(rooms) {
    const el = document.getElementById('rooms');
    el.innerHTML = rooms.map(r => `
        <div class="room-card">
            <div>
                <span class="room-name">${esc(r.name || 'Untitled')}</span>
                <span class="room-meta"> &middot; ${r.players}/2 players &middot; ${r.status}</span>
            </div>
            <div class="room-actions">
                <button class="btn primary join-btn" onclick="joinRoom('${r.id}')" ${r.players >= 2 ? 'disabled' : ''}>
                    ${r.players >= 2 ? 'Full' : 'Join'}
                </button>
                ${r.status === 'playing' && r.players >= 2
                    ? `<button class="btn secondary watch-btn" onclick="watchRoom('${r.id}')">Watch</button>`
                    : ''}
            </div>
        </div>
    `).join('');
}

function watchRoom(id) {
    window.location.href = '/game.html?room=' + encodeURIComponent(id) + '&mode=spectator';
}

function playAgainstComputer() {
    if (!playerName) {
        showError('Enter a display name first.');
        return;
    }
    window.location.href = '/game.html?mode=ai&name=' + encodeURIComponent(playerName);
}

async function createRoom() {
    if (!playerName) {
        showError('Enter a display name first.');
        return;
    }
    // Each room this user creates gets a new sequential suffix so room names
    // stay unique (e.g. "Sam's room #2") and never collide with their earlier
    // creations. The server room id remains unique regardless.
    const seq = (parseInt(localStorage.getItem(ROOM_SEQ_STORAGE_KEY), 10) || 0) + 1;
    localStorage.setItem(ROOM_SEQ_STORAGE_KEY, String(seq));
    const roomName = playerName + "'s room #" + seq;
    try {
        const res = await fetch(API + '/create', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: roomName }),
        });
        const room = await res.json();
        if (room.id) {
            window.location.href = '/game.html?room=' + room.id + '&name=' + encodeURIComponent(playerName);
        }
    } catch (e) {
        showError('Failed to create room: ' + e.message);
    }
}

function joinRoom(id) {
    if (!playerName) {
        showError('Enter a display name first.');
        return;
    }
    fetch(API + '/join', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ room_id: id }),
    }).then(r => r.json()).then(data => {
        if (data.error) {
            showError(data.error);
            return;
        }
        window.location.href = '/game.html?room=' + id + '&name=' + encodeURIComponent(playerName) + '&mode=' + (data.mode || 'local');
    }).catch(e => showError(e.message));
}

function showError(msg) {
    const el = document.getElementById('error');
    el.textContent = msg;
    el.classList.remove('hidden');
    setTimeout(() => el.classList.add('hidden'), 4000);
}

function esc(s) {
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}