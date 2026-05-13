import { describe, it, expect } from 'vitest'
import {
  userRoomEvent,
  roomEvent,
  memberAdd,
  memberRemove,
  memberRoleUpdate,
  searchRooms,
  searchMessages,
  msgSurrounding,
  readReceipt,
} from './subjects'

describe('subjects', () => {
  it('userRoomEvent builds the per-user room event subject', () => {
    expect(userRoomEvent('alice')).toBe('chat.user.alice.event.room')
  })

  it('roomEvent still builds the per-room subject', () => {
    expect(roomEvent('r1')).toBe('chat.room.r1.event')
  })

  it('memberAdd builds the add-member request subject', () => {
    expect(memberAdd('alice', 'r1', 'site-A')).toBe(
      'chat.user.alice.request.room.r1.site-A.member.add'
    )
  })

  it('memberRemove builds the remove-member request subject', () => {
    expect(memberRemove('alice', 'r1', 'site-A')).toBe(
      'chat.user.alice.request.room.r1.site-A.member.remove'
    )
  })

  it('memberRoleUpdate builds the role-update request subject', () => {
    expect(memberRoleUpdate('alice', 'r1', 'site-A')).toBe(
      'chat.user.alice.request.room.r1.site-A.member.role-update'
    )
  })

  it('searchRooms builds the search rooms request subject', () => {
    expect(searchRooms('alice')).toBe('chat.user.alice.request.search.rooms')
  })

  it('searchMessages builds the search messages request subject', () => {
    expect(searchMessages('alice')).toBe('chat.user.alice.request.search.messages')
  })

  it('msgSurrounding builds the surrounding-messages request subject', () => {
    expect(msgSurrounding('alice', 'r1', 'site-A')).toBe(
      'chat.user.alice.request.room.r1.site-A.msg.surrounding'
    )
  })

  it('readReceipt builds the request subject for the read-receipt RPC', () => {
    expect(readReceipt('alice', 'room1', 'site1')).toBe(
      'chat.user.alice.request.room.room1.site1.message.read-receipt'
    )
  })
})
