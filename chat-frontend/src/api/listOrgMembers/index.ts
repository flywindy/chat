import { orgMembers } from '../_transport/subjects'
import type { Nats } from '../types'

export interface ListOrgMembersArgs {
  orgId: string
}

export interface OrgMember {
  id: string
  account: string
  engName?: string
  chineseName?: string
  siteId: string
}

export interface ListOrgMembersResponse {
  members: OrgMember[]
}

/** Expand an org (sect) into its individual members. */
export async function listOrgMembers(
  { user, request }: Nats,
  { orgId }: ListOrgMembersArgs,
): Promise<ListOrgMembersResponse> {
  return request<ListOrgMembersResponse>(orgMembers(user.account, orgId), {})
}
