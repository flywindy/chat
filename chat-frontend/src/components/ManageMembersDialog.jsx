import { useState } from 'react'
import AddMembersForm from './manageMembers/AddMembersForm'
import MemberRoster from './manageMembers/MemberRoster'

const TABS = [
  { id: 'roster', label: 'Members' },
  { id: 'add', label: 'Add' },
]

export default function ManageMembersDialog({ room, onClose }) {
  const [mode, setMode] = useState('roster')

  return (
    <div className="dialog-overlay" onClick={onClose}>
      <div className="dialog manage-members-dialog" onClick={(e) => e.stopPropagation()}>
        <h2>Manage Members — {room.name}</h2>

        <div className="manage-members-tabs" role="tablist">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              role="tab"
              aria-selected={mode === t.id}
              className={`manage-members-tab${mode === t.id ? ' manage-members-tab-active' : ''}`}
              onClick={() => setMode(t.id)}
            >
              {t.label}
            </button>
          ))}
        </div>

        {mode === 'roster' ? <MemberRoster room={room} /> : <AddMembersForm room={room} />}

        <div className="dialog-actions manage-members-footer">
          <button type="button" className="dialog-cancel" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  )
}
